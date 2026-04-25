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

func cmdSpawn(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: camux spawn <session> [flags] — see 'camux spawn -h'")
	}
	session := args[0]
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	winName := fs.String("name", "cc", "window name (e.g. 'cc', 'planner')")
	dir := fs.String("dir", "", "launch cwd (becomes Claude's working directory)")
	noSkip := fs.Bool("no-skip-perms", false, "omit --dangerously-skip-permissions")
	timeout := fs.Duration("timeout", 60*time.Second, "ready deadline")

	// Passthrough flags mapped to `claude` CLI flags.
	model := fs.String("model", "", "model alias or ID (claude --model)")
	systemPrompt := fs.String("system-prompt", "", "full system prompt (claude --system-prompt)")
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

	// Build the window command: first amux's args, then "--", then
	// claude + every passthrough flag.
	windowArgs := []string{"window", session, "-n", *winName, "--", claudeBin}
	if !*noSkip {
		windowArgs = append(windowArgs, "--dangerously-skip-permissions")
	}
	if *dir != "" {
		windowArgs = append(windowArgs, "--add-dir", *dir)
	}
	if *model != "" {
		windowArgs = append(windowArgs, "--model", *model)
	}
	if *systemPrompt != "" {
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
		case StateTheme:
			// Theme picker: option 1 = Auto. Accepting any theme is
			// fine — the user can change later via /theme.
			if err := chooseOption(target, "1"); err != nil {
				return err
			}
			time.Sleep(500 * time.Millisecond)
		case StateLogin:
			// Login picker: option 1 = Claude account with subscription.
			// The orch will already have valid OAuth via the keychain
			// shim, so this should be a no-op transition past the dialog.
			if err := chooseOption(target, "1"); err != nil {
				return err
			}
			time.Sleep(500 * time.Millisecond)
		case StateBypassPerms:
			// Bypass-permissions consent: option 2 = "Yes, I accept".
			// Option 1 is "No, exit" — the only menu where 1 is NOT
			// the accepting choice. Spawn is invoked with
			// --dangerously-skip-permissions on purpose, so accepting
			// matches user intent.
			if err := chooseOption(target, "2"); err != nil {
				return err
			}
			time.Sleep(500 * time.Millisecond)
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
