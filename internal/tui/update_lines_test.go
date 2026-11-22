package tui

import (
	"strings"
	"testing"

	"monopanel/internal/apitypes"
)

// After an install that rolled back, the Updates screen says what a retry
// does, as mp update and the web page do; other attempts get no such line.
func TestUpdateLinesSayWhatARetryDoes(t *testing.T) {
	st := &apitypes.UpdateStatus{Current: "0.9.0", Latest: "0.9.1", Available: true,
		Settings:    apitypes.UpdateSettings{Repo: "acme/panel", Channel: "stable", CheckHours: 24},
		LastAttempt: &apitypes.UpdateAttempt{Status: "rolled-back", From: "0.9.0", To: "0.9.1", Reinstall: true}}
	text := strings.Join(updateLines(st), "\n")
	if !strings.Contains(text, "The package database lists 0.9.1 already while the previous binary runs: a (mp update apply) installs it once more") {
		t.Fatalf("no word of the retry:\n%s", text)
	}
	st.LastAttempt.Reinstall = false
	if text := strings.Join(updateLines(st), "\n"); strings.Contains(text, "once more") {
		t.Fatalf("a retry named without a reinstall to do:\n%s", text)
	}
}
