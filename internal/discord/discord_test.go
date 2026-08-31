package discord

import (
	"os"
	"strings"
	"testing"
)

func TestBoardURLTargetsSessionWithoutAddingAToken(t *testing.T) {
	old := os.Getenv("AMAC_BOARD_URL")
	t.Cleanup(func() { _ = os.Setenv("AMAC_BOARD_URL", old) })
	if err := os.Setenv("AMAC_BOARD_URL", "https://amac.example/board?view=compact"); err != nil {
		t.Fatal(err)
	}
	got := BoardURL("work one")
	if !strings.Contains(got, "session=work+one") || !strings.Contains(got, "view=compact") {
		t.Fatalf("link did not preserve the base query and target the session: %s", got)
	}
	if strings.Contains(got, "token") {
		t.Fatalf("notification link leaked a token: %s", got)
	}
}

func TestHandoffURLTargetsMacWithoutAddingAToken(t *testing.T) {
	old := os.Getenv("AMAC_BOARD_URL")
	t.Cleanup(func() { _ = os.Setenv("AMAC_BOARD_URL", old) })
	if err := os.Setenv("AMAC_BOARD_URL", "https://amac.example/"); err != nil {
		t.Fatal(err)
	}
	oldSecret := os.Getenv("AMAC_HANDOFF_SECRET")
	t.Cleanup(func() { _ = os.Setenv("AMAC_HANDOFF_SECRET", oldSecret) })
	_ = os.Setenv("AMAC_HANDOFF_SECRET", "test-secret")
	got := HandoffURL("am-work one")
	if !strings.Contains(got, "/handoff?") || !strings.Contains(got, "session=am-work+one") || !strings.Contains(got, "sig=") {
		t.Fatalf("handoff link missing its target: %s", got)
	}
	if strings.Contains(got, "token") {
		t.Fatalf("handoff link leaked a token: %s", got)
	}
}
