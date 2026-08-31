package handoff

import (
	"testing"
	"time"
)

func TestCapabilityIsScopedAndExpires(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	expires := now.Add(Lifetime)
	sig := Sign("secret", "am-work", expires)
	if !Valid("secret", "am-work", "1800000600", sig, now) {
		t.Fatal("fresh capability rejected")
	}
	for _, tc := range []struct {
		secret, session, expires, sig string
		now                           time.Time
	}{
		{"other", "am-work", "1800000600", sig, now},
		{"secret", "am-other", "1800000600", sig, now},
		{"secret", "am-work", "1800000601", sig, now},
		{"secret", "am-work", "1800000600", sig, expires},
	} {
		if Valid(tc.secret, tc.session, tc.expires, tc.sig, tc.now) {
			t.Fatal("modified or expired capability accepted")
		}
	}
}
