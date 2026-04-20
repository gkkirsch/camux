package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// findClaudeBin resolves the claude binary. Honors $CLAUDE_BIN, otherwise
// uses `claude` from PATH.
func findClaudeBin() (string, error) {
	if b := os.Getenv("CLAUDE_BIN"); b != "" {
		return b, nil
	}
	// Prefer the resolved path so tmux doesn't see a shell alias.
	p, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("claude binary not found on PATH (set CLAUDE_BIN)")
	}
	return p, nil
}

// --- spawn ------------------------------------------------------------------

func cmdSpawn(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: camux spawn <session> [--name W] [--dir D] [--no-skip-perms] [--timeout 60s]")
	}
	session := args[0]
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	winName := fs.String("name", "cc", "window name")
	dir := fs.String("dir", "", "directory to launch Claude in (becomes its cwd)")
	noSkip := fs.Bool("no-skip-perms", false, "omit --dangerously-skip-permissions")
	timeout := fs.Duration("timeout", 60*time.Second, "time to wait for Claude to become ready")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	claudeBin, err := findClaudeBin()
	if err != nil {
		return err
	}

	// Create the session if it doesn't already exist.
	if !amuxExists(session) {
		if _, err := runAmux("new", session); err != nil {
			return err
		}
	}
	target := session + ":" + *winName

	// Build the window command.
	windowArgs := []string{"window", session, "-n", *winName, "--", claudeBin}
	if !*noSkip {
		windowArgs = append(windowArgs, "--dangerously-skip-permissions")
	}
	if *dir != "" {
		windowArgs = append(windowArgs, "--add-dir", *dir)
	}
	if _, err := runAmux(windowArgs...); err != nil {
		return err
	}

	// Drive Claude to Ready state, handling the trust dialog if it appears.
	deadline := time.Now().Add(*timeout)
	attempts := 0
	for time.Now().Before(deadline) {
		st, _, err := currentState(target)
		if err != nil {
			return err
		}
		switch st {
		case StateReady:
			fmt.Println(target)
			return nil
		case StateTrust:
			// Default selection is "Yes, I trust this folder" — just press Enter.
			if _, err := runAmux("key", target, "Enter"); err != nil {
				return err
			}
			// Give the TUI a beat to redraw before polling again.
			time.Sleep(400 * time.Millisecond)
		case StatePermission:
			return fmt.Errorf("spawn: unexpected permission dialog before first use on %s — did Claude prompt for something?", target)
		case StateNotFound:
			return fmt.Errorf("spawn: window %s disappeared", target)
		default:
			// starting / streaming (unlikely on fresh spawn) — keep polling.
		}
		time.Sleep(300 * time.Millisecond)
		attempts++
	}
	_, cap, _ := currentState(target)
	return fmt.Errorf("spawn: %s never reached ready state within %s. Last capture tail:\n%s",
		target, *timeout, lastLines(cap, 15))
}

// --- ask --------------------------------------------------------------------

func cmdAsk(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: camux ask <target> [--timeout 180s] [--interval 400ms] < prompt")
	}
	target := args[0]
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	timeout := fs.Duration("timeout", 180*time.Second, "overall timeout for the response")
	interval := fs.Duration("interval", 400*time.Millisecond, "poll interval for state transitions")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if !amuxExists(target) {
		return fmt.Errorf("ask: no such target %s", target)
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("ask: read stdin: %w", err)
	}
	if len(bytes.TrimSpace(prompt)) == 0 {
		return fmt.Errorf("ask: stdin was empty")
	}

	// Require Ready to submit. If the target is streaming, in a dialog, or
	// starting up, the orchestrator should resolve that first (via `status`,
	// `trust`, `permit`, `interrupt`).
	st, cap, err := currentState(target)
	if err != nil {
		return err
	}
	if st != StateReady {
		return fmt.Errorf("ask: %s is in state %q, not ready. Handle with camux %s first.\nLast capture tail:\n%s",
			target, st, suggestedCmd(st), lastLines(cap, 10))
	}

	// Snapshot the pane's line offset BEFORE submit so we can emit the reply
	// delta at the end. We use amux's display-message via list --json? No —
	// we need the raw offset. Shell out to tmux directly.
	beforeOffset, err := paneLineOffset(target)
	if err != nil {
		return err
	}

	// Submit via amux paste --submit (bracketed, sanitized).
	pasteCmd := exec.Command(amuxBinName, "paste", target, "--submit")
	pasteCmd.Stdin = bytes.NewReader(prompt)
	var errb bytes.Buffer
	pasteCmd.Stderr = &errb
	if err := pasteCmd.Run(); err != nil {
		return fmt.Errorf("ask: paste failed: %s", strings.TrimSpace(errb.String()))
	}

	// Wait for Claude to enter streaming (response started) or, if the
	// response is so short it never shows "esc to interrupt", for the
	// content to change meaningfully. Give it up to ~4 seconds.
	enteredStreaming := false
	streamWatchDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(streamWatchDeadline) {
		st, _, _ := currentState(target)
		if st == StateStreaming {
			enteredStreaming = true
			break
		}
		// If Claude somehow finished instantly, break too.
		if st == StateReady && time.Since(streamWatchDeadline.Add(-4*time.Second)) > 800*time.Millisecond {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Now wait for "not streaming" (ready or dialog or dead).
	overallDeadline := time.Now().Add(*timeout)
	for time.Now().Before(overallDeadline) {
		st, cap, err := currentState(target)
		if err != nil {
			return err
		}
		switch st {
		case StateReady:
			return emitDelta(target, beforeOffset)
		case StatePermission:
			return fmt.Errorf("ask: paused on permission dialog on %s. Resolve with 'camux permit %s [yes|no]'.\nLast capture tail:\n%s",
				target, target, lastLines(cap, 10))
		case StateTrust:
			return fmt.Errorf("ask: trust dialog appeared mid-ask on %s (unexpected). Resolve with 'camux trust %s'.", target, target)
		case StateNotFound, StateDead:
			return fmt.Errorf("ask: target %s disappeared mid-response", target)
		}
		time.Sleep(*interval)
	}
	_ = enteredStreaming // suppress unused in simple path
	return fmt.Errorf("ask: timed out after %s waiting for %s to finish streaming", *timeout, target)
}

// paneLineOffset shells out to tmux to read history_size + cursor_y. Kept
// here (not in amux.go) because it's a small enough helper and we don't
// want to add a new amux subcommand just for this.
func paneLineOffset(target string) (int, error) {
	out, err := exec.Command("tmux", "display-message", "-p", "-t", target,
		"#{history_size} #{cursor_y}").Output()
	if err != nil {
		return 0, fmt.Errorf("tmux display-message: %w", err)
	}
	var hs, cy int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d", &hs, &cy); err != nil {
		return 0, fmt.Errorf("parse offset %q: %w", string(out), err)
	}
	return hs + cy, nil
}

func emitDelta(target string, beforeOffset int) error {
	hsOut, err := exec.Command("tmux", "display-message", "-p", "-t", target, "#{history_size}").Output()
	if err != nil {
		return err
	}
	var hs int
	fmt.Sscanf(strings.TrimSpace(string(hsOut)), "%d", &hs)
	rel := beforeOffset - hs
	cmd := exec.Command("tmux", "capture-pane", "-p", "-t", target, "-S", fmt.Sprint(rel))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return err
	}
	fmt.Print(out.String())
	return nil
}

// --- status -----------------------------------------------------------------

func cmdStatus(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: camux status <target>")
	}
	target := args[0]
	st, _, err := currentState(target)
	if err != nil {
		return err
	}
	fmt.Println(string(st))
	// Non-ready states exit non-zero so scripts can branch on it.
	switch st {
	case StateReady:
		return nil
	case StateNotFound:
		os.Exit(2)
	default:
		os.Exit(1)
	}
	return nil
}

// --- trust / permit / interrupt / clear / slash ----------------------------

func cmdTrust(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: camux trust <target>")
	}
	target := args[0]
	st, cap, err := currentState(target)
	if err != nil {
		return err
	}
	if st != StateTrust {
		// No-op, friendly — orchestrators call this defensively.
		fmt.Fprintf(os.Stderr, "camux: trust: %s not in trust dialog (state=%s)\n", target, st)
		_ = cap
		return nil
	}
	// Default selection is option 1 "Yes, I trust this folder".
	if _, err := runAmux("key", target, "Enter"); err != nil {
		return err
	}
	// Give the TUI a beat and confirm we're past the dialog.
	time.Sleep(400 * time.Millisecond)
	st2, _, _ := currentState(target)
	if st2 == StateTrust {
		return fmt.Errorf("trust: dialog still up on %s after Enter", target)
	}
	return nil
}

func cmdPermit(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: camux permit <target> [yes|no|always]")
	}
	target := args[0]
	choice := "yes"
	if len(args) >= 2 {
		choice = args[1]
	}
	st, _, err := currentState(target)
	if err != nil {
		return err
	}
	if st != StatePermission {
		fmt.Fprintf(os.Stderr, "camux: permit: %s not in permission dialog (state=%s)\n", target, st)
		return nil
	}
	// Permission dialogs typically have a list; Claude's default selection
	// is "yes". We approximate by sending Down to reach "no"/"always" and
	// Enter. Since the exact layout depends on the dialog, this is a best
	// effort — for complex multi-choice dialogs, orchestrators should use
	// `amux key` directly.
	downs := 0
	switch choice {
	case "yes", "y":
		downs = 0
	case "always", "a":
		downs = 1
	case "no", "n":
		downs = 2
	default:
		return fmt.Errorf("permit: unknown choice %q (want yes|no|always)", choice)
	}
	for i := 0; i < downs; i++ {
		if _, err := runAmux("key", target, "Down"); err != nil {
			return err
		}
		time.Sleep(80 * time.Millisecond)
	}
	if _, err := runAmux("key", target, "Enter"); err != nil {
		return err
	}
	return nil
}

func cmdInterrupt(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: camux interrupt <target>")
	}
	target := args[0]
	if !amuxExists(target) {
		return fmt.Errorf("interrupt: no such target %s", target)
	}
	_, err := runAmux("key", target, "Escape")
	return err
}

func cmdClear(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: camux clear <target>")
	}
	target := args[0]
	if !amuxExists(target) {
		return fmt.Errorf("clear: no such target %s", target)
	}
	// Claude's own shortcut: two quick Escapes = clear input.
	_, err := runAmux("key", target, "Escape", "Escape")
	return err
}

func cmdSlash(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: camux slash <target> <slashcmd> [--no-enter] [--delay 80ms]")
	}
	target := args[0]
	cmd := args[1]
	fs := flag.NewFlagSet("slash", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	noEnter := fs.Bool("no-enter", false, "don't press Enter after typing (useful before menu navigation)")
	delay := fs.Duration("delay", 80*time.Millisecond, "delay between chars when typing the command")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if !amuxExists(target) {
		return fmt.Errorf("slash: no such target %s", target)
	}
	// Type char-by-char — the slash menu filters as you type, and rich
	// TUIs often treat bulk sends as pastes (wrong target).
	text := "/" + cmd
	if _, err := runAmux("type", target, text, "--delay", delay.String()); err != nil {
		return err
	}
	if !*noEnter {
		// Brief beat so the menu can filter/select.
		time.Sleep(200 * time.Millisecond)
		if _, err := runAmux("key", target, "Enter"); err != nil {
			return err
		}
	}
	return nil
}

// --- helpers ----------------------------------------------------------------

func suggestedCmd(st ClaudeState) string {
	switch st {
	case StateStreaming:
		return "interrupt"
	case StateTrust:
		return "trust"
	case StatePermission:
		return "permit"
	case StateStarting:
		return "(wait — Claude is still starting)"
	case StateNotFound:
		return "spawn"
	}
	return "status"
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
