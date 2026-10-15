package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIndependentExactGrantCannotEscapeSymlink(t *testing.T) {
	for _, parent := range []bool{false, true} {
		name := "target_symlink"
		if parent {
			name = "parent_symlink"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			outside := filepath.Join(dir, "outside")
			if err := os.Mkdir(outside, 0700); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(outside, "victim.txt")
			if err := os.WriteFile(victim, []byte("must remain unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "authorized.txt")
			if parent {
				alias := filepath.Join(dir, "authorized-parent")
				if err := os.Symlink(outside, alias); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(alias, "victim.txt")
			} else if err := os.Symlink(victim, target); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AMAC_FAKE_RESULTS", filepath.Join(dir, "results.json"))
			t.Setenv("AMAC_FAKE_WRITE", target)
			t.Setenv("AMAC_FAKE_DENIED_WRITE", filepath.Join(dir, "denied.txt"))
			sup := withFakeAgent(t, "fsops")
			defer sup.Shutdown()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			sess, err := sup.Start(ctx, "claude", dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.AuthorizeWrite(WriteScope{SessionID: sess.ID, AuthorizationID: "exact-only", Path: target, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
				return
			}
			if _, err := sess.Prompt(ctx, "do file things"); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(victim)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != "must remain unchanged" {
				t.Fatalf("exact-path grant escaped via symlink: ungranted victim overwritten with %q", b)
			}
		})
	}
}
