package main

import (
	"fmt"
	"regexp"
	"time"
)

// ClaudeState describes what Claude Code's TUI is doing right now. camux's
// commands are state-machine-aware — `ask` refuses to submit while a dialog
// is up, `status` reports it, `trust` and `permit` dismiss them.
type ClaudeState string

const (
	StateReady      ClaudeState = "ready"
	StateStreaming  ClaudeState = "streaming"
	StateTrust      ClaudeState = "trust-dialog"
	StatePermission ClaudeState = "permission-dialog"
	StateStarting   ClaudeState = "starting"
	StateNotFound   ClaudeState = "not-found"
	StateDead       ClaudeState = "dead"
)

// Patterns for state detection. All gleaned from observing Claude Code's
// real TUI output. Order in detectState matters — more specific first.
var (
	reTrustDialog = regexp.MustCompile(`(?i)Yes, I trust this folder|Quick safety check`)
	// Permission dialogs take many forms. The most reliable markers are
	// variants of "Do you want to <verb>" and "Claude requested
	// permissions". The "allow all edits during this session" option text
	// is another unique signature that appears in most write/edit dialogs.
	rePermissionDialog = regexp.MustCompile(`(?i)Do you want to\s|Claude requested permissions|allow all edits during this session`)
	reStreamingStatus  = regexp.MustCompile(`esc to interrupt`)
	reReadyPromptBar   = regexp.MustCompile(`⏵⏵ bypass permissions on|\? for shortcuts`)
	reReadyPromptLine  = regexp.MustCompile(`(?m)^\s*❯\s*$`)
	reWelcomeBanner    = regexp.MustCompile(`Claude Code v\d+`)
)

// detectState classifies a capture of Claude's TUI. Order matters: dialogs
// are strictly more specific than streaming/ready, and streaming is
// strictly more specific than ready (both can co-exist visually).
//
// Ready is the DEFAULT once we've ruled out dialogs and streaming and the
// TUI status bar is visible — the empty-input-line regex turned out to be
// fragile across tmux versions and column widths.
func detectState(capture string) ClaudeState {
	switch {
	case reTrustDialog.MatchString(capture):
		return StateTrust
	case rePermissionDialog.MatchString(capture):
		return StatePermission
	case reStreamingStatus.MatchString(capture):
		return StateStreaming
	case reReadyPromptBar.MatchString(capture):
		return StateReady
	case reWelcomeBanner.MatchString(capture):
		return StateStarting
	default:
		return StateStarting
	}
}

// currentState returns the live state of a Claude target, or NotFound if
// the pane doesn't exist.
func currentState(target string) (ClaudeState, string, error) {
	if !amuxExists(target) {
		return StateNotFound, "", nil
	}
	cap, err := capture(target, 200)
	if err != nil {
		return StateNotFound, "", err
	}
	return detectState(cap), cap, nil
}

// waitForState blocks until the target is in any of `want` states, or
// times out. Between polls it sleeps `interval`. Useful building block
// for spawn (wait for ready) and ask (wait for not-streaming).
func waitForState(target string, want []ClaudeState, timeout, interval time.Duration) (ClaudeState, string, error) {
	deadline := time.Now().Add(timeout)
	var lastState ClaudeState
	var lastCap string
	for {
		st, cap, err := currentState(target)
		if err != nil {
			return st, cap, err
		}
		lastState, lastCap = st, cap
		for _, w := range want {
			if st == w {
				return st, cap, nil
			}
		}
		if time.Now().After(deadline) {
			return lastState, lastCap, fmt.Errorf("waitForState: timed out after %s on %s (last state: %s)", timeout, target, lastState)
		}
		time.Sleep(interval)
	}
}
