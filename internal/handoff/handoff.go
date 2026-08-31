// Package handoff creates narrowly scoped, short-lived capabilities for moving
// one session from a phone notification to Terminal on the Mac. It deliberately
// does not expose the board token, which can type into every session.
package handoff

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

const Lifetime = 10 * time.Minute

func Sign(secret, session string, expires time.Time) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(session))
	m.Write([]byte("\n"))
	m.Write([]byte(strconv.FormatInt(expires.Unix(), 10)))
	return hex.EncodeToString(m.Sum(nil))
}

func Valid(secret, session, rawExpires, signature string, now time.Time) bool {
	unix, err := strconv.ParseInt(rawExpires, 10, 64)
	if err != nil || secret == "" || session == "" || signature == "" {
		return false
	}
	expires := time.Unix(unix, 0)
	// Reject capabilities minted for an implausibly distant future as well as
	// expired ones, so a leaked URL cannot quietly become permanent.
	if !expires.After(now) || expires.After(now.Add(Lifetime+time.Minute)) {
		return false
	}
	want, err := hex.DecodeString(Sign(secret, session, expires))
	if err != nil {
		return false
	}
	got, err := hex.DecodeString(signature)
	return err == nil && hmac.Equal(got, want)
}
