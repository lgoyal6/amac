package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/apply"
	"github.com/lgoyal6/amac/internal/event"
)

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	emailFile := fs.String("email", "", "parse a raw confirmation email from this file (- for stdin)")
	company := fs.String("company", "", "record manually: company")
	role := fs.String("role", "", "record manually: role")
	url := fs.String("url", "", "record manually: url")
	list := fs.Bool("list", false, "list tracked applications")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()

	if *list {
		return listApplications(log)
	}

	// The Notion sink is optional. Detection is the hard part and must work
	// standalone, so a missing token degrades to "recorded locally" rather
	// than refusing the whole command.
	var sink apply.Sink
	if n, err := apply.NewNotion(); err == nil {
		sink = n
	} else {
		fmt.Printf("note: %v (recording locally only)\n\n", err)
	}
	tracker := apply.NewTracker(log, sink)

	var app apply.Application
	switch {
	case *emailFile != "":
		raw, err := readAll(*emailFile)
		if err != nil {
			return err
		}
		subject, from, body := splitEmail(raw)
		var ok bool
		app, ok = apply.FromEmail(subject, from, body, time.Now())
		if !ok {
			return fmt.Errorf("this does not read like an application confirmation; nothing recorded")
		}
	case *company != "":
		app = apply.Application{
			Company: *company, Role: orStr(*role, "Unspecified"), URL: *url,
			Source: apply.SourceExtension, AppliedAt: time.Now(),
		}
		if ats, ok := apply.DetectATS(*url); ok {
			app.ATS = ats
		}
	default:
		return fmt.Errorf("usage: amac apply -email FILE | -company X [-role Y] [-url Z] | -list")
	}

	isNew, err := tracker.Record(context.Background(), app)
	if err != nil {
		return err
	}
	status := "already tracked"
	if isNew {
		status = "recorded"
	}
	fmt.Printf("%s: %s / %s", status, app.Company, app.Role)
	if app.ATS != "" {
		fmt.Printf(" (%s)", app.ATS)
	}
	fmt.Printf("\nkey %s\n", app.Key())
	return nil
}

func listApplications(log *event.Log) error {
	rows, err := log.DB().Query(`
		SELECT json_extract(payload,'$.company'), json_extract(payload,'$.role'),
		       json_extract(payload,'$.ats'), json_extract(payload,'$.source'), at
		FROM events
		WHERE kind='application' AND json_extract(payload,'$.duplicate')=0
		ORDER BY seq DESC LIMIT 50`)
	if err != nil {
		return err
	}
	defer rows.Close()

	n := 0
	fmt.Printf("%-24s %-30s %-14s %-10s %s\n", "COMPANY", "ROLE", "ATS", "SOURCE", "WHEN")
	for rows.Next() {
		var c, r, a, s, at *string
		if err := rows.Scan(&c, &r, &a, &s, &at); err != nil {
			return err
		}
		when := ""
		if at != nil {
			if t, err := time.Parse(time.RFC3339Nano, *at); err == nil {
				when = t.Local().Format("Jan02 15:04")
			}
		}
		fmt.Printf("%-24s %-30s %-14s %-10s %s\n",
			trunc(deref(c), 24), trunc(deref(r), 30), deref(a), deref(s), when)
		n++
	}
	if n == 0 {
		fmt.Println("\nnone yet")
	}
	return rows.Err()
}

// splitEmail pulls headers out of a raw message. Deliberately simple: the
// parser only needs Subject and From, and full MIME handling would be a
// dependency for no gain.
func splitEmail(raw string) (subject, from, body string) {
	parts := strings.SplitN(strings.ReplaceAll(raw, "\r\n", "\n"), "\n\n", 2)
	head := parts[0]
	if len(parts) > 1 {
		body = parts[1]
	}
	for _, line := range strings.Split(head, "\n") {
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "subject:"):
			subject = strings.TrimSpace(line[len("subject:"):])
		case strings.HasPrefix(lower, "from:"):
			from = strings.TrimSpace(line[len("from:"):])
		}
	}
	if subject == "" && body == "" {
		body = raw // not a raw message; treat the whole thing as body
	}
	return
}

func readAll(path string) (string, error) {
	if path == "-" {
		b, err := os.ReadFile("/dev/stdin")
		return string(b), err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
