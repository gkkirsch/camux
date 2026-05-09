package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

func jsonUnmarshalOrEmpty(b []byte, v any) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

func jsonEncode(w io.Writer, v any) error {
	e := json.NewEncoder(w)
	return e.Encode(v)
}

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

// captureCmd runs a command and returns a single-line diagnostic
// string of "exit=<n> out=<combined output, newlines→' | '>". Used
// by spawn to embed multiple probe results in one error message.
func captureCmd(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	s := strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", " | ")
	if s == "" {
		s = "(empty)"
	}
	return fmt.Sprintf("exit=%d out=%q", exitCode, s)
}

func cmdSpawn(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: camux spawn <session> [flags] — see 'camux spawn -h'")
	}
	session := args[0]
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	winName := fs.String("name", "cc", "window name (e.g. 'cc', 'planner')")
	dir := fs.String("dir", "", "extra dir Claude is allowed to read (claude --add-dir)")
	cwd := fs.String("cwd", "", "tmux window's working directory — Claude's $PWD")
	noSkip := fs.Bool("no-skip-perms", false, "omit --dangerously-skip-permissions")
	timeout := fs.Duration("timeout", 60*time.Second, "ready deadline")

	// Passthrough flags mapped to `claude` CLI flags.
	model := fs.String("model", "", "model alias or ID (claude --model)")
	systemPrompt := fs.String("system-prompt", "", "full system prompt as a string (claude --system-prompt). Long prompts with shell metacharacters get mangled by tmux's default-shell parsing — prefer --system-prompt-file.")
	systemPromptFile := fs.String("system-prompt-file", "", "path to a file containing the system prompt (claude --system-prompt-file). Avoids the shell-escaping pitfalls of passing the content inline.")
	appendSystem := fs.String("append-system", "", "append to default system prompt (claude --append-system-prompt)")
	effort := fs.String("effort", "", "effort level: low|medium|high|xhigh|max (claude --effort)")
	permMode := fs.String("permission-mode", "", "permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan")
	displayName := fs.String("display-name", "", "display name shown inside Claude's TUI (claude --name)")
	sessionID := fs.String("session-id", "", "reuse a specific Claude session UUID")
	resume := fs.String("resume", "", "resume a past conversation by session ID")
	continueLast := fs.Bool("continue", false, "continue the most recent conversation in cwd")
	agents := fs.String("agents", "", "JSON object defining custom agents (claude --agents)")

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

	// Decide whether to create a fresh window or co-opt an existing one.
	// Three cases:
	//   1. No existing window  → create fresh, wait for ready.
	//   2. Live existing window → co-opt it. Covers the case where a prior
	//                             spawn created the window and claude reached
	//                             ready but the caller didn't observe success
	//                             (process killed mid-flight, transient probe
	//                             false-negative). Without this the live
	//                             orphan permanently blocks every retry.
	//   3. Dead pane from a prior attempt → kill it and create fresh in this
	//                             same call. If claude's still consistently
	//                             crashing, the new attempt's StateDead error
	//                             will surface the buffer; if it's transient,
	//                             we recover transparently.
	//
	// Strict-match (amuxExists ∨ tmuxWindowExists) avoids stacking duplicate
	// 'cc' windows when amux and tmux briefly disagree about visibility.
	skipCreate := false
	if amuxExists(target) || tmuxWindowExists(session, *winName) {
		if paneIsDead(target) {
			_ = exec.Command("tmux", "kill-window", "-t", target).Run()
		} else {
			skipCreate = true
		}
	}

	// Build the window command: first amux's args, then "--", then
	// claude + every passthrough flag.
	windowArgs := []string{"window", session, "-n", *winName}
	if *cwd != "" {
		windowArgs = append(windowArgs, "-c", *cwd)
	}
	windowArgs = append(windowArgs, "--", claudeBin)
	if !*noSkip {
		windowArgs = append(windowArgs, "--dangerously-skip-permissions")
	}
	if *dir != "" {
		windowArgs = append(windowArgs, "--add-dir", *dir)
	}
	if *model != "" {
		windowArgs = append(windowArgs, "--model", *model)
	}
	if *systemPromptFile != "" {
		windowArgs = append(windowArgs, "--system-prompt-file", *systemPromptFile)
	} else if *systemPrompt != "" {
		windowArgs = append(windowArgs, "--system-prompt", *systemPrompt)
	}
	if *appendSystem != "" {
		windowArgs = append(windowArgs, "--append-system-prompt", *appendSystem)
	}
	if *effort != "" {
		windowArgs = append(windowArgs, "--effort", *effort)
	}
	if *permMode != "" {
		windowArgs = append(windowArgs, "--permission-mode", *permMode)
	}
	if *displayName != "" {
		windowArgs = append(windowArgs, "--name", *displayName)
	}
	if *sessionID != "" {
		windowArgs = append(windowArgs, "--session-id", *sessionID)
	}
	if *resume != "" {
		windowArgs = append(windowArgs, "--resume", *resume)
	}
	if *continueLast {
		windowArgs = append(windowArgs, "--continue")
	}
	if *agents != "" {
		windowArgs = append(windowArgs, "--agents", *agents)
	}
	if !skipCreate {
		if err := createWindow(session, *winName, target, windowArgs); err != nil {
			return err
		}
	}
	return waitForReady(target, *timeout)
}

// createWindow opens the new amux window with `remain-on-exit` pinned
// onto it, then waits until the window is visible to a strict probe.
//
// Why remain-on-exit is mandatory: when claude crashes faster than tmux
// can default-handle it (auth bug, --resume to a missing session, bad
// arg), `remain-on-exit on` keeps the dead pane around so the spawn
// loop can read its buffer and report what happened. Without it, tmux
// reaps the window and we'd surface "vanished" with no useful detail.
//
// Why post-create visibility check: amux returning success means tmux's
// command server accepted new-window, but a follow-up tmux client
// connection (the wait loop's first poll) can briefly observe the
// pre-creation state. Polling for visibility here rather than once
// inside the wait loop keeps the loop's contract simple — by the time
// it runs, the window definitely exists.
func createWindow(session, winName, target string, windowArgs []string) error {
	var prev string
	if out, err := exec.Command("tmux", "show-options", "-gv", "remain-on-exit").Output(); err == nil {
		prev = strings.TrimSpace(string(out))
	}
	if err := exec.Command("tmux", "set-option", "-g", "remain-on-exit", "on").Run(); err != nil {
		return fmt.Errorf("spawn: failed to enable tmux remain-on-exit before window creation: %w", err)
	}
	defer restoreRemainOnExit(prev)

	if _, err := runAmux(windowArgs...); err != nil {
		return err
	}
	if err := waitForWindowVisible(session, winName, target, 500*time.Millisecond); err != nil {
		return err
	}
	if err := exec.Command("tmux", "set-window-option", "-t", target, "remain-on-exit", "on").Run(); err != nil {
		return fmt.Errorf("spawn: window %s created but failed to pin remain-on-exit on it: %w", target, err)
	}
	return nil
}

func restoreRemainOnExit(prev string) {
	if prev != "" {
		_ = exec.Command("tmux", "set-option", "-g", "remain-on-exit", prev).Run()
	} else {
		_ = exec.Command("tmux", "set-option", "-gu", "remain-on-exit").Run()
	}
}

func waitForWindowVisible(session, winName, target string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		if amuxExists(target) || tmuxWindowExists(session, winName) {
			return nil
		}
		if time.Now().After(deadline) {
			lw := captureCmd("tmux", "list-windows", "-t", session, "-F",
				"idx=#{window_index} name=#{window_name} dead=#{pane_dead}")
			return fmt.Errorf("spawn: amux reported success creating %s but the window did not appear within %s. tmux list-windows -t %s → %s",
				target, within, session, lw)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForReady drives the Claude TUI from the moment its window exists to
// the moment Claude is ready for input, dismissing first-launch dialogs
// along the way. It does not own cleanup: a window that vanishes
// mid-flight returns a transition error, not a self-heal kill — the
// lifecycle layer (roster) decides what to do about orphan state.
func waitForReady(target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, cap, err := currentState(target)
		if err != nil {
			return err
		}
		switch st {
		case StateReady:
			fmt.Println(target)
			return nil
		case StateDead:
			return fmt.Errorf("spawn: claude exited inside %s before reaching ready. Pane buffer:\n%s", target, lastLines(cap, 30))
		case StateNotFound:
			return fmt.Errorf("spawn: window %s vanished after creation. claude likely crashed faster than tmux could honor remain-on-exit", target)
		case StateTrust:
			if _, err := runAmux("key", target, "Enter"); err != nil {
				return err
			}
			time.Sleep(400 * time.Millisecond)
		case StateTheme:
			if err := chooseOption(target, "1"); err != nil {
				return err
			}
			time.Sleep(500 * time.Millisecond)
		case StateLogin:
			if err := chooseOption(target, "1"); err != nil {
				return err
			}
			time.Sleep(500 * time.Millisecond)
		case StateBypassPerms:
			// Option 2 = "Yes, I accept". Option 1 = "No, exit" — the only
			// menu where 1 is not the accepting choice.
			if err := chooseOption(target, "2"); err != nil {
				return err
			}
			time.Sleep(500 * time.Millisecond)
		case StatePermission:
			return fmt.Errorf("spawn: unexpected permission dialog before first use on %s", target)
		}
		time.Sleep(300 * time.Millisecond)
	}
	_, cap, _ := currentState(target)
	return fmt.Errorf("spawn: %s never reached ready state within %s. Last capture tail:\n%s",
		target, timeout, lastLines(cap, 15))
}

// chooseOption picks option N in a Claude TUI menu by typing the digit
// and pressing Enter. Used by spawn to auto-handle first-launch dialogs
// (theme, login, bypass-perms) and by the standalone `choose` command
// for any menu the operator wants to drive from the CLI.
func chooseOption(target, n string) error {
	if _, err := runAmux("send", target, n); err != nil {
		return fmt.Errorf("type option %q: %w", n, err)
	}
	if _, err := runAmux("key", target, "Enter"); err != nil {
		return fmt.Errorf("press Enter after option %q: %w", n, err)
	}
	return nil
}

// --- ask --------------------------------------------------------------------

func cmdAsk(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: camux ask <target> [--timeout 180s] [--interval 400ms] [--auto-permit MODE] [--auto-trust] < prompt")
	}
	target := args[0]
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	timeout := fs.Duration("timeout", 180*time.Second, "overall timeout for the response")
	interval := fs.Duration("interval", 400*time.Millisecond, "poll interval for state transitions")
	autoPermit := fs.String("auto-permit", "", "auto-answer permission dialogs mid-response: yes|no|always (default: bail with error)")
	autoTrust := fs.Bool("auto-trust", false, "auto-dismiss trust dialogs mid-response")
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
			if *autoPermit != "" {
				if err := cmdPermit([]string{target, *autoPermit}); err != nil {
					return fmt.Errorf("ask: auto-permit failed: %w", err)
				}
				time.Sleep(500 * time.Millisecond)
				continue
			}
			return fmt.Errorf("ask: paused on permission dialog on %s. Re-run with --auto-permit yes, or resolve with 'camux permit %s [yes|no|always]' then use 'camux wait'.\nLast capture tail:\n%s",
				target, target, lastLines(cap, 10))
		case StateTrust:
			if *autoTrust {
				if err := cmdTrust([]string{target}); err != nil {
					return fmt.Errorf("ask: auto-trust failed: %w", err)
				}
				time.Sleep(500 * time.Millisecond)
				continue
			}
			return fmt.Errorf("ask: trust dialog appeared mid-ask on %s. Re-run with --auto-trust, or resolve with 'camux trust %s'.", target, target)
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

// --- plugin / auth / sessions (wrappers around claude subcommands) ---------

// cmdPlugin wraps `claude plugin <subcmd>`. We don't reimplement any plugin
// logic — we just streamline the invocation and forward flags/args.
func cmdPlugin(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: camux plugin <list|install|uninstall|enable|disable|update|marketplace> [args...]")
	}
	claudeBin, err := findClaudeBin()
	if err != nil {
		return err
	}
	cmdArgs := append([]string{"plugin"}, args...)
	cmd := exec.Command(claudeBin, cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cmdAuth wraps `claude auth <subcmd>`. Auth itself may need interaction
// (login flow) — we pass stdin/stdout/stderr through so the user can
// complete the flow manually when needed.
func cmdAuth(args []string) error {
	claudeBin, err := findClaudeBin()
	if err != nil {
		return err
	}
	cmdArgs := append([]string{"auth"}, args...)
	cmd := exec.Command(claudeBin, cmdArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cmdSessions enumerates amux sessions that look Claude-ish (a window's
// command contains "claude" or the pane's Claude state is detectable).
// Reports per-session pane target + state, so orchestrators can see
// everything camux has spawned plus any hand-started Claudes.
func cmdSessions(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit JSON")
	allPanes := fs.Bool("all", false, "include non-Claude panes too")
	if err := fs.Parse(args); err != nil {
		return err
	}
	out, err := runAmux("list", "--json")
	if err != nil {
		return err
	}
	type paneInfo struct {
		Session    string `json:"session"`
		Window     int    `json:"window"`
		WindowName string `json:"window_name"`
		Pane       int    `json:"pane"`
		PID        int    `json:"pid"`
		Active     bool   `json:"active"`
		Command    string `json:"command"`
	}
	var panes []paneInfo
	if err := jsonUnmarshalOrEmpty([]byte(out), &panes); err != nil {
		return fmt.Errorf("parse amux list json: %w", err)
	}
	type row struct {
		Target  string      `json:"target"`
		PID     int         `json:"pid"`
		Command string      `json:"command"`
		State   ClaudeState `json:"state"`
	}
	var rows []row
	for _, p := range panes {
		target := fmt.Sprintf("%s:%d.%d", p.Session, p.Window, p.Pane)
		claudeish := isClaudeCommand(p.Command)
		if !claudeish && !*allPanes {
			continue
		}
		st := StateStarting
		if claudeish {
			if s, _, _ := currentState(target); s != "" {
				st = s
			}
		} else {
			st = ClaudeState("non-claude")
		}
		rows = append(rows, row{Target: target, PID: p.PID, Command: p.Command, State: st})
	}
	if *asJSON {
		return jsonEncode(os.Stdout, rows)
	}
	if len(rows) == 0 {
		fmt.Println("(no sessions)")
		return nil
	}
	for _, r := range rows {
		fmt.Printf("%-30s  pid=%-7d  state=%-18s  (%s)\n", r.Target, r.PID, r.State, r.Command)
	}
	return nil
}

func isClaudeCommand(cmd string) bool {
	s := strings.ToLower(cmd)
	// tmux shows the process's argv[0] which can be "claude", "node
	// claude", a version string like "2.1.116", or a path. Best-effort.
	if strings.Contains(s, "claude") {
		return true
	}
	// Claude Code's command label often shows its version, e.g. "2.1.116".
	if regexp.MustCompile(`^\d+\.\d+\.\d+`).MatchString(s) {
		return true
	}
	return false
}

// cmdWait blocks until the target is Ready, automatically resolving any
// dialogs it passes through. Useful after an `ask` that bailed on a
// permission dialog: answer it once, then `wait` until the task finishes.
func cmdWait(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: camux wait <target> [--timeout 180s] [--auto-permit MODE] [--auto-trust]")
	}
	target := args[0]
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	timeout := fs.Duration("timeout", 180*time.Second, "overall timeout")
	interval := fs.Duration("interval", 400*time.Millisecond, "poll interval")
	autoPermit := fs.String("auto-permit", "yes", "auto-answer permission dialogs: yes|no|always|off")
	autoTrust := fs.Bool("auto-trust", true, "auto-dismiss trust dialogs")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !amuxExists(target) {
		return fmt.Errorf("wait: no such target %s", target)
	}
	deadline := time.Now().Add(*timeout)
	for {
		st, cap, err := currentState(target)
		if err != nil {
			return err
		}
		switch st {
		case StateReady:
			fmt.Println("ready")
			return nil
		case StatePermission:
			if *autoPermit == "off" {
				return fmt.Errorf("wait: permission dialog on %s and --auto-permit=off", target)
			}
			if err := cmdPermit([]string{target, *autoPermit}); err != nil {
				return err
			}
			time.Sleep(400 * time.Millisecond)
		case StateTrust:
			if !*autoTrust {
				return fmt.Errorf("wait: trust dialog on %s and --auto-trust=false", target)
			}
			if err := cmdTrust([]string{target}); err != nil {
				return err
			}
			time.Sleep(400 * time.Millisecond)
		case StateNotFound:
			return fmt.Errorf("wait: target %s disappeared", target)
		default:
			// streaming / starting — just keep polling
			_ = cap
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait: timed out after %s on %s (last state: %s)", *timeout, target, st)
		}
		time.Sleep(*interval)
	}
}

// cmdReload runs /reload-plugins inside the TUI to pick up plugin changes
// without restarting Claude. Prints the TUI's summary line.
func cmdReload(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: camux reload <target>")
	}
	target := args[0]
	if !amuxExists(target) {
		return fmt.Errorf("reload: no such target %s", target)
	}
	// Snapshot before so we can emit just the reload result line.
	before, err := paneLineOffset(target)
	if err != nil {
		return err
	}
	if _, err := runAmux("type", target, "/reload-plugins", "--delay", "30ms"); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := runAmux("key", target, "Enter"); err != nil {
		return err
	}
	// Wait for the "Reloaded: ..." output line, with a short timeout.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, hs, _ := paneOffsetAndHistory(target)
		rel := before - hs
		out, _ := exec.Command("tmux", "capture-pane", "-p", "-t", target,
			"-S", fmt.Sprint(rel)).Output()
		if m := regexp.MustCompile(`Reloaded:.*`).FindString(string(out)); m != "" {
			fmt.Println(m)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("reload: didn't see reload confirmation in 10s")
}

// paneOffsetAndHistory returns (hs+cy, hs). Pulled here so cmdReload
// doesn't need a tmux round-trip for just hs when paneLineOffset already
// fetched them together.
func paneOffsetAndHistory(target string) (int, int, error) {
	out, err := exec.Command("tmux", "display-message", "-p", "-t", target,
		"#{history_size} #{cursor_y}").Output()
	if err != nil {
		return 0, 0, err
	}
	var hs, cy int
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d", &hs, &cy)
	return hs + cy, hs, nil
}

// cmdInfo runs /status in the TUI and parses the key fields. Prints
// human-readable by default, --json for structured output. The session
// ID (UUID) is one of the fields — useful for orchestrators that want
// to resume sessions via `spawn --resume`.
func cmdInfo(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: camux info <target> [--json]")
	}
	target := args[0]
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !amuxExists(target) {
		return fmt.Errorf("info: no such target %s", target)
	}
	// Snapshot before the slash so we can isolate /status output.
	before, err := paneLineOffset(target)
	if err != nil {
		return err
	}
	if _, err := runAmux("type", target, "/status", "--delay", "30ms"); err != nil {
		return err
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := runAmux("key", target, "Enter"); err != nil {
		return err
	}
	// Poll capture until we see the Session ID line appear.
	deadline := time.Now().Add(10 * time.Second)
	var cap string
	for time.Now().Before(deadline) {
		_, hs, _ := paneOffsetAndHistory(target)
		rel := before - hs
		out, _ := exec.Command("tmux", "capture-pane", "-p", "-t", target,
			"-S", fmt.Sprint(rel)).Output()
		if strings.Contains(string(out), "Session ID:") {
			cap = string(out)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if cap == "" {
		return fmt.Errorf("info: /status didn't produce a Session ID line in 10s")
	}
	// Dismiss the overlay (two Escapes is Claude's clear shortcut).
	_, _ = runAmux("key", target, "Escape", "Escape")

	// Parse fields out of the captured text.
	info := parseStatusFields(cap)
	info["target"] = target
	if *asJSON {
		return jsonEncode(os.Stdout, info)
	}
	for _, k := range []string{"version", "session_id", "session_name", "cwd", "login_method", "organization", "email", "model", "mcp_servers"} {
		if v := info[k]; v != "" {
			fmt.Printf("%-14s %s\n", k+":", v)
		}
	}
	return nil
}

func parseStatusFields(s string) map[string]string {
	out := map[string]string{}
	scan := func(key, label string) {
		// lines look like `  Session ID:       9270e246-…`
		re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(label) + `\s*(.+?)\s*$`)
		if m := re.FindStringSubmatch(s); m != nil {
			out[key] = strings.TrimSpace(m[1])
		}
	}
	scan("version", "Version:")
	scan("session_name", "Session name:")
	scan("session_id", "Session ID:")
	scan("cwd", "cwd:")
	scan("login_method", "Login method:")
	scan("organization", "Organization:")
	scan("email", "Email:")
	scan("model", "Model:")
	scan("mcp_servers", "MCP servers:")
	scan("setting_sources", "Setting sources:")
	return out
}

// cmdModel switches the model in-session by typing /model <name>. We use
// slash underneath so the TUI filter sees each keystroke.
func cmdModel(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: camux model <target> <model>  (e.g. sonnet, opus, haiku-4-5)")
	}
	target, model := args[0], args[1]
	if !amuxExists(target) {
		return fmt.Errorf("model: no such target %s", target)
	}
	// Type "/model <name>" char-by-char, then Enter.
	if _, err := runAmux("type", target, "/model "+model, "--delay", "60ms"); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := runAmux("key", target, "Enter"); err != nil {
		return err
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
