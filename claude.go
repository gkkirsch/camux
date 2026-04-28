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
	StateReady       ClaudeState = "ready"
	StateStreaming   ClaudeState = "streaming"
	StateTrust       ClaudeState = "trust-dialog"
	StatePermission  ClaudeState = "permission-dialog"
	StateTheme       ClaudeState = "theme-dialog"
	StateLogin       ClaudeState = "login-dialog"
	StateBypassPerms ClaudeState = "bypass-perms-dialog"
	StateStarting    ClaudeState = "starting"
	StateNotFound    ClaudeState = "not-found"
	StateDead        ClaudeState = "dead"
)

// Patterns for state detection. All gleaned from observing Claude Code's
// real TUI output. Order in detectState matters — more specific first.
var (
	reTrustDialog = regexp.MustCompile(`(?i)Yes, I trust this folder|Quick safety check`)
	// Permission dialogs take many forms. We detect them by combining
	// two signals:
	//   1. A trigger phrase ("Do you want to", "Claude requested
	//      permissions", "allow all edits during this session").
	//   2. A numbered-options block ("1. Yes", "2. No", …) — every
	//      Claude Code permission modal renders one. Requiring both
	//      avoids false positives when scrollback echoes the orch's
	//      OWN text (e.g. an assistant message with "What do you want
	//      to do?") that matches the trigger but isn't a live prompt.
	rePermissionTrigger = regexp.MustCompile(`(?i)Do you want to\s|Claude requested permissions|allow all edits during this session`)
	rePermissionOptions = regexp.MustCompile(`(?m)^[│|\s]*(?:❯\s*)?[1-5]\.\s+\S`)
	// Modal dialogs in Claude Code's TUI are framed with box-drawing
	// characters. Plain assistant replies that happen to contain "do
	// you want to" + a numbered list do not. Requiring the box top OR
	// bottom corner anchors detection to actual modals.
	rePermissionFrame = regexp.MustCompile(`╭─|╰─|│ Do you want to`)
	// First-launch dialogs (when CLAUDE_CONFIG_DIR is fresh and we
	// haven't seeded onboarding state). roster's prepareClaudeIsolation
	// normally skips all of these via .claude.json + settings.json
	// seeding; these patterns are the safety net for direct-camux
	// spawns or seeding edge cases.
	reThemeDialog       = regexp.MustCompile(`Choose the text style|Dark mode \(colorblind`)
	reLoginDialog       = regexp.MustCompile(`Select login method|Claude account with subscription`)
	reBypassPermsDialog = regexp.MustCompile(`Bypass Permissions mode|Yes, I accept`)
	reStreamingStatus   = regexp.MustCompile(`esc to interrupt`)
	reReadyPromptBar    = regexp.MustCompile(`⏵⏵ bypass permissions on|\? for shortcuts`)
	reReadyPromptLine   = regexp.MustCompile(`(?m)^\s*❯\s*$`)
	reWelcomeBanner     = regexp.MustCompile(`Claude Code v\d+`)
)

// detectState classifies a capture of Claude's TUI. Order matters: dialogs
// are strictly more specific than streaming/ready, and streaming is
// strictly more specific than ready (both can co-exist visually).
//
// Ready is the DEFAULT once we've ruled out dialogs and streaming and the
// TUI status bar is visible — the empty-input-line regex turned out to be
// fragile across tmux versions and column widths.
//
// Critical: "esc to interrupt" is a live status-bar indicator that only
// appears at the very bottom of the screen while streaming. Old tool
// outputs and "Interrupted" notes can leave that exact substring in
// scrollback for hundreds of lines after the orch returned to idle. We
// only look at the last few lines for the streaming + ready signals.
// Dialogs scan the whole capture (they redraw every frame so a stale
// match isn't possible).
func detectState(capture string) ClaudeState {
	tail := lastLines(capture, 8)
	// Permission/trust dialogs overlay the input area at the bottom of
	// the screen. Scoping their detection to a slightly larger window
	// keeps a multi-line modal in view but prevents stale matches
	// buried in scrollback from misclassifying the agent.
	dialogTail := lastLines(capture, 20)
	switch {
	// First-launch dialogs are checked BEFORE the ready prompt bar:
	// the ready bar can persist visually in capture buffers when a
	// modal overlays the input area.
	case reBypassPermsDialog.MatchString(capture):
		return StateBypassPerms
	case reLoginDialog.MatchString(capture):
		return StateLogin
	case reThemeDialog.MatchString(capture):
		return StateTheme
	case reTrustDialog.MatchString(dialogTail):
		return StateTrust
	case rePermissionTrigger.MatchString(dialogTail) &&
		rePermissionOptions.MatchString(dialogTail) &&
		rePermissionFrame.MatchString(dialogTail):
		return StatePermission
	case reStreamingStatus.MatchString(tail):
		return StateStreaming
	case reReadyPromptBar.MatchString(tail):
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
