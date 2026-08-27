package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// within is the security boundary for every read here, and this endpoint is
// reachable from a phone with a token. The interesting case is the symlink: a
// prefix check on the unresolved string passes for a link that sits inside the
// root and points anywhere on the disk, which is the whole trick.
func TestWithinStaysInsideTheSessionDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	// Traversal is clamped rather than rejected: Clean("/"+rel) makes ".." at
	// the root a no-op, so these land on the root itself or on a path under it
	// that simply does not exist.
	for _, rel := range []string{"", ".", "ok.txt", "sub", "../..", "/etc/passwd", "../../.ssh"} {
		got, ok := within(root, rel)
		if !ok {
			t.Errorf("within(%q) refused outright; expected it clamped inside", rel)
			continue
		}
		real, _ := filepath.EvalSymlinks(root)
		if got != real && !filepath.HasPrefix(got, real+string(os.PathSeparator)) {
			t.Errorf("within(%q) = %q, which is outside %q", rel, got, real)
		}
	}

	// The one that must be refused rather than clamped, because it resolves
	// cleanly to somewhere else entirely.
	if got, ok := within(root, "escape/secret"); ok {
		t.Fatalf("a symlink out of the root must be refused, got %q", got)
	}
}
