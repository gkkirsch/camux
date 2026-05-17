package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
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
	// Anchors detection to a real live modal vs. plain assistant text.
	// Two known framings:
	//   1. Box-drawing modal:  ╭─/╰─/│  (the standard tool-use prompt)
	//   2. Horizontal-rule + footer:  the "Claude requested permissions
	//      to edit … which is a sensitive file" prompt has a thin
	//      "Esc to cancel · Tab to amend" footer line and a horizontal
	//      ─── divider above the message instead of a full box. Both
	//      footer fragments are unique to live modals.
	rePermissionFrame = regexp.MustCompile(`╭─|╰─|│ Do you want to|Esc to cancel.*Tab to amend|sensitive file`)
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
// the window doesn't exist. When spawn enables `remain-on-exit`, an exited
// claude process leaves the pane in the "dead" state — we report
// StateDead with the captured buffer so callers can include it in error
// messages instead of losing it to tmux's reap.
//
// Strictness: tmux's `-t session:name` resolution is fuzzy and falls back
// to the active window when `name` doesn't exist. We verify the returned
// window matches what we asked for (by name OR index, whichever the
// caller used) so a missing target reads as NotFound instead of resolving
// to whatever else tmux had on hand.
//
// Race tolerance: if the window vanishes between display-message and
// capture-pane (a normal occurrence when something is killing windows
// concurrently), we report NotFound rather than leaking tmux's stderr.
func currentState(target string) (ClaudeState, string, error) {
	_, want := splitTarget(target)
	cmd := exec.Command("tmux", "display-message", "-p", "-t", target,
		"#{window_name}\t#{window_index}\t#{pane_dead}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		traceLog("currentState.display-message.error",
			"target", target, "want", want,
			"err", err.Error(),
			"out", strings.TrimSpace(string(out)))
		return StateNotFound, "", nil
	}
	raw := strings.TrimSpace(string(out))
	parts := strings.SplitN(raw, "\t", 3)
	if len(parts) != 3 {
		traceLog("currentState.parse.short",
			"target", target, "want", want,
			"raw", raw, "len", fmt.Sprint(len(parts)))
		return StateNotFound, "", nil
	}
	if parts[0] != want && parts[1] != want {
		// tmux's display-message falls back to the active window when the
		// requested target name doesn't exist yet — and returns exit 0
		// with the fallback window's data. Strict-match correctly rejects
		// it, but the fallback fires during the post-new-window race
		// before display-message's resolver has seen the new window
		// (which list-windows ALREADY sees, because that's a different
		// code path that updates synchronously). Rather than declare
		// NotFound and tear everything down, double-check with
		// list-windows: if it sees our window, keep polling.
		sess, _ := splitTarget(target)
		if tmuxWindowExists(sess, want) {
			traceLog("currentState.race-fallback",
				"target", target, "want", want,
				"displayMessageName", parts[0], "displayMessageIdx", parts[1],
				"listWindowsSays", "present")
			return StateStarting, "", nil
		}
		traceLog("currentState.mismatch",
			"target", target, "want", want,
			"name", parts[0], "idx", parts[1], "dead", parts[2])
		return StateNotFound, "", nil
	}
	dead := parts[2] == "1"
	cap, err := exec.Command("tmux", "capture-pane", "-p", "-t", target, "-S", "-200").Output()
	if err != nil {
		traceLog("currentState.capture-pane.error", "target", target, "err", err.Error())
		return StateNotFound, "", nil
	}
	if dead {
		return StateDead, string(cap), nil
	}
	return detectState(string(cap)), string(cap), nil
}

// splitTarget splits a "session:window" target. Returns ("", "") if there's
// no colon — callers treat that as a session-only target.
func splitTarget(target string) (sess, win string) {
	i := strings.Index(target, ":")
	if i < 0 {
		return target, ""
	}
	return target[:i], target[i+1:]
}

// paneIsDead reports whether tmux has marked the pane as dead (the
// process inside it exited but the pane is still around because
// `remain-on-exit on` was set). Best-effort: if tmux doesn't respond,
// treat it as alive and let the normal state machine continue.
func paneIsDead(target string) bool {
	out, err := exec.Command("tmux", "display-message", "-p", "-t", target, "#{pane_dead}").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "1"
}

// tmuxWindowExists is a backstop check that bypasses amux entirely
// and asks tmux directly whether the named window exists in the
// session. We use this in cmdSpawn alongside amuxExists because
// amux's wrapper has been observed to return false negatives in
// some environments (different tmux server reachable to amux vs. the
// one holding the window, stale PATH-resolved amux, .tmux.conf
// hooks). False here means "either tmux says no, or tmux is broken
// — the caller should still attempt to spawn." Match window NAME
// exactly (no prefix matching) so this tracks amux's contract.
func tmuxWindowExists(session, winName string) bool {
	out, err := exec.Command("tmux", "list-windows", "-t", session, "-F", "#{window_name}").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == winName {
			return true
		}
	}
	return false
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
