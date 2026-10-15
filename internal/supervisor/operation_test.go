package supervisor

import (
	pathpkg "path"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteAuthorityRequiresCurrentIdentityAndExpiry(t *testing.T) {
	sess := &Session{ID: "session-current"}
	path := filepath.Join(t.TempDir(), "result.txt")
	valid := WriteScope{
		SessionID: "session-current", AuthorizationID: "approval-1",
		Path: path, ExpiresAt: time.Now().Add(time.Minute),
	}

	wrongSession := valid
	wrongSession.SessionID = "session-previous"
	if err := sess.AuthorizeWrite(wrongSession); err == nil {
		t.Fatal("authority from a previous session was accepted")
	}

	expired := valid
	expired.ExpiresAt = time.Now().Add(-time.Second)
	if err := sess.AuthorizeWrite(expired); err == nil {
		t.Fatal("expired authority was accepted")
	}

	unclean := valid
	unclean.Path = pathpkg.Dir(path) + "/nested/../" + filepath.Base(path)
	if err := sess.AuthorizeWrite(unclean); err == nil {
		t.Fatal("non-canonical scope path was accepted")
	}

	if err := sess.AuthorizeWrite(valid); err != nil {
		t.Fatalf("valid exact-path authority rejected: %v", err)
	}
	if !writeScopeAllows(&valid, sess.ID, path, time.Now()) {
		t.Fatal("current exact-path authority was not recognized")
	}
	if writeScopeAllows(&valid, sess.ID, path+".other", time.Now()) {
		t.Fatal("authority was accepted for a different path")
	}
	if writeScopeAllows(&valid, sess.ID, path, valid.ExpiresAt) {
		t.Fatal("authority remained valid at its expiry")
	}
	if err := sess.RevokeWrite(valid.AuthorizationID); err != nil {
		t.Fatalf("revoke current grant: %v", err)
	}
	if writeScopeAllows(sess.writeScope, sess.ID, path, time.Now()) {
		t.Fatal("revoked authorization remained active")
	}
}
