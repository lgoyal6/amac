package health

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/discord"
)

// Delivery reuses the Discord bot agentmon already registered: same token in
// the login keychain, same cached DM channel. Standing up a second bot would
// mean a second token to rotate and a second DM thread to check, for a channel
// that is already proven to reach his phone.
// Send delivers one message to the DM channel.
func Send(ctx context.Context, content string) error { return discord.Send(ctx, content) }

// Digest renders the full roster for a phone screen.
//
// Discord's mobile column is roughly forty characters, so the layout spends
// none of it on indentation and gives detail only where detail is wanted: a
// healthy automation is one line, and everything below the fold is reserved
// for the ones that need him. Reports arrive worst-first from Run.
func Digest(reports []Report) string {
	var bad, good []Report
	for _, r := range reports {
		if r.State == OK {
			good = append(good, r)
		} else {
			bad = append(bad, r)
		}
	}

	var b strings.Builder
	if len(bad) == 0 {
		fmt.Fprintf(&b, "✅ **Automations** · all %d delivering\n", len(reports))
	} else {
		fmt.Fprintf(&b, "⚠️ **Automations** · %d of %d need attention\n", len(bad), len(reports))
	}

	for _, r := range bad {
		text, link := splitURL(r.Detail)
		fmt.Fprintf(&b, "\n%s **%s** · %s\n%s\n", r.State.Icon(), r.Name, r.State, text)
		if !r.Last.IsZero() {
			fmt.Fprintf(&b, "· last delivery %s\n", r.Last.Local().Format("Mon 15:04"))
		}
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "· %s\n", tidy(n))
		}
		if r.Err != "" {
			fmt.Fprintf(&b, "· %s\n", tidy(r.Err))
		}
		if link != "" {
			fmt.Fprintf(&b, "%s\n", link)
		}
	}

	if len(good) > 0 {
		if len(bad) > 0 {
			b.WriteString("\n**Healthy**\n")
		} else {
			b.WriteString("\n")
		}
		// No per-line icon here. Under a Healthy heading every one of them
		// would be the same green dot, and two columns of emoji width is the
		// difference between these lines fitting a phone and wrapping.
		for _, r := range good {
			fmt.Fprintf(&b, "**%s** · %s\n", r.Name, tidy(r.Detail))
		}
	}

	fmt.Fprintf(&b, "\n_checked %s_", time.Now().Local().Format("Mon 2 Jan 15:04"))
	return b.String()
}

// bareURL matches an unwrapped http(s) URL.
var bareURL = regexp.MustCompile(`https?://[^\s<>]+`)

// tidy wraps URLs in angle brackets, which is what stops Discord expanding a
// link into a preview card. On a phone one card pushes the rest of the roster
// off the screen, and these links are for tapping, not for reading.
func tidy(s string) string {
	return bareURL.ReplaceAllString(strings.TrimSpace(s), "<$0>")
}

// splitURL lifts a trailing link out of a detail string onto its own line.
// Inline, a GitHub Actions run URL is longer than the sentence carrying it and
// wraps across four phone lines, burying the reason the automation is red.
func splitURL(s string) (text, link string) {
	s = strings.TrimSpace(s)
	loc := bareURL.FindStringIndex(s)
	if loc == nil {
		return s, ""
	}
	text = strings.TrimRight(strings.TrimSpace(s[:loc[0]]), " ·,:")
	link = "<" + s[loc[0]:loc[1]] + ">"
	if rest := strings.TrimSpace(s[loc[1]:]); rest != "" {
		text = strings.TrimSpace(text + " " + rest)
	}
	return text, link
}

// Alert renders only what changed, so a healthy day is silent. A monitor that
// messages every cycle gets muted, and a muted monitor is worse than none.
//
// "Changed" means the state differs, not merely that it is bad. A pipeline
// moving from Late to Failing has told him something new: it stopped being
// quiet and started erroring, and comparing against OK alone would swallow
// that.
func Alert(reports []Report, prev map[string]State) (string, bool) {
	var broke, fixed []Report
	for _, r := range reports {
		was, seen := prev[r.Name]
		switch {
		case r.State != OK && (!seen || was != r.State):
			broke = append(broke, r)
		case r.State == OK && seen && was != OK:
			fixed = append(fixed, r)
		}
	}
	if len(broke) == 0 && len(fixed) == 0 {
		return "", false
	}
	var b strings.Builder
	for _, r := range broke {
		text, link := splitURL(r.Detail)
		fmt.Fprintf(&b, "%s **%s** · %s\n%s\n", r.State.Icon(), r.Name, r.State, text)
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "· %s\n", tidy(n))
		}
		if link != "" {
			fmt.Fprintf(&b, "%s\n", link)
		}
	}
	for _, r := range fixed {
		fmt.Fprintf(&b, "🟢 **%s recovered** · %s\n", r.Name, tidy(r.Detail))
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// RunBatch renders every run seen this sweep as one message.
//
// Every completed run is included, but runs discovered in the same health
// sweep share one message so Discord remains an activity journal without
// producing a burst of separate phone notifications.
func RunBatch(runs []Run) string {
	var b strings.Builder
	if len(runs) == 1 {
		b.WriteString("**Run**\n")
	} else {
		fmt.Fprintf(&b, "**Runs** · %d\n", len(runs))
	}
	for _, r := range runs {
		fmt.Fprintf(&b, "%s **%s**", r.Status.Icon(), r.Automation)
		if !r.Started.IsZero() {
			fmt.Fprintf(&b, " · %s", r.Started.Local().Format("15:04"))
		}
		fmt.Fprintf(&b, " · %s", r.Detail)
		if r.Duration > 0 {
			fmt.Fprintf(&b, " · %s", short(r.Duration))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// RunFailure is sent on its own, so a failure is never a line in a list he
// skims. Everything else can wait for the batch.
func RunFailure(r Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔴 **%s run failed** · %s\n", r.Automation, r.Detail)
	fmt.Fprintf(&b, "started %s", r.Started.Local().Format("Mon 15:04"))
	if r.Duration > 0 {
		fmt.Fprintf(&b, ", ran %s", short(r.Duration))
	}
	if r.URL != "" {
		fmt.Fprintf(&b, "\n<%s>", r.URL)
	}
	return b.String()
}
