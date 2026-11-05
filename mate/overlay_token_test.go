package mate

import "testing"

// TestUpdateTokenErrorsWhenNotRunning guards the Swift fallback path: when the
// overlay is not running, UpdateToken must report an error so the caller falls
// back to a full restart rather than silently believing the token was applied.
func TestUpdateTokenErrorsWhenNotRunning(t *testing.T) {
	m := NewOverlayManager()
	if err := m.UpdateToken("new-token"); err == nil {
		t.Fatal("expected error when overlay is not running, got nil")
	}
}

// TestUpdateTokenSwapsAccessToken verifies the in-place swap: a running,
// configured manager accepts a new token, ignores a blank one, and treats an
// unchanged token as a no-op (still success, no error).
func TestUpdateTokenSwapsAccessToken(t *testing.T) {
	m := NewOverlayManager()
	m.config = &OverlayConfig{AccessToken: "old-token"}
	m.running.Store(true)
	t.Cleanup(func() { m.running.Store(false) })

	if err := m.UpdateToken(""); err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
	if got := m.config.AccessToken; got != "old-token" {
		t.Fatalf("empty token must not overwrite; got %q", got)
	}

	if err := m.UpdateToken("old-token"); err != nil {
		t.Fatalf("unchanged token should be a no-op, got error: %v", err)
	}

	if err := m.UpdateToken("new-token"); err != nil {
		t.Fatalf("valid token swap failed: %v", err)
	}
	if got := m.config.AccessToken; got != "new-token" {
		t.Fatalf("token not swapped; got %q want %q", got, "new-token")
	}
}
