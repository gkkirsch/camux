package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	camuxBin string
	amuxPath string
)

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Fprintln(os.Stderr, "SKIP: tmux not on PATH")
		os.Exit(0)
	}
	ap, err := exec.LookPath("amux")
	if err != nil {
		fmt.Fprintln(os.Stderr, "SKIP: amux not on PATH")
		os.Exit(0)
	}
	amuxPath = ap

	tmp, err := os.MkdirTemp("", "camux-build-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer os.RemoveAll(tmp)
	camuxBin = filepath.Join(tmp, "camux")
	build := exec.Command("go", "build", "-o", camuxBin, ".")
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// --- helpers --------------------------------------------------------------

type result struct {
	stdout, stderr string
	exit           int
}

func runCamux(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(camuxBin, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("run camux %v: %v", args, err)
		}
	}
	return result{out.String(), errb.String(), exit}
}

func killAmuxSession(name string) {
	exec.Command(amuxPath, "kill", name).Run()
}

// --- unit: detectState ----------------------------------------------------

func TestDetectStateTrust(t *testing.T) {
	cap := "Quick safety check: Is this a project you created?\n  1. Yes, I trust this folder\n  2. No, exit"
	if got := detectState(cap); got != StateTrust {
		t.Fatalf("want trust, got %s", got)
	}
}

func TestDetectStateStreaming(t *testing.T) {
	cap := "Claude Code v2.1\n⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt"
	if got := detectState(cap); got != StateStreaming {
		t.Fatalf("want streaming, got %s", got)
	}
}

func TestDetectStateReady(t *testing.T) {
	cap := "Claude Code v2.1.116\n❯ \n⏵⏵ bypass permissions on (shift+tab to cycle)"
	if got := detectState(cap); got != StateReady {
		t.Fatalf("want ready, got %s", got)
	}
}

func TestParseStatusFields(t *testing.T) {
	s := `
   Status   Config   Usage   Stats
  Version:          2.1.116
  Session name:     /rename to add a name
  Session ID:       9270e246-660b-420e-8dce-5ce700d92b0d
  cwd:              /Users/gkkirsch/dev/camux
  Login method:     Claude Max account
  Organization:     Garrett Kirschbaum
  Email:            ibekidkirsch@gmail.com
  Model:            Default Opus 4.7 with 1M context · Most capable
  MCP servers:      2 connected, 3 need auth, 1 failed · /mcp
  Setting sources:  User settings
`
	got := parseStatusFields(s)
	want := map[string]string{
		"version":         "2.1.116",
		"session_id":      "9270e246-660b-420e-8dce-5ce700d92b0d",
		"cwd":             "/Users/gkkirsch/dev/camux",
		"login_method":    "Claude Max account",
		"organization":    "Garrett Kirschbaum",
		"email":           "ibekidkirsch@gmail.com",
		"setting_sources": "User settings",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parseStatusFields[%s] = %q, want %q", k, got[k], v)
		}
	}
	if !strings.Contains(got["model"], "Opus 4.7") {
		t.Errorf("parseStatusFields[model] = %q, want contains 'Opus 4.7'", got["model"])
	}
	if !strings.Contains(got["mcp_servers"], "2 connected") {
		t.Errorf("parseStatusFields[mcp_servers] = %q, want contains '2 connected'", got["mcp_servers"])
	}
}

func TestIsClaudeCommand(t *testing.T) {
	cases := map[string]bool{
		"claude":      true,
		"node claude": true,
		"2.1.116":     true,
		"2.0.0":       true,
		"bash":        false,
		"zsh":         false,
		"vim":         false,
	}
	for in, want := range cases {
		if got := isClaudeCommand(in); got != want {
			t.Errorf("isClaudeCommand(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDetectStateStarting(t *testing.T) {
	if got := detectState("random shell prompt"); got != StateStarting {
		t.Fatalf("want starting (default), got %s", got)
	}
}

// TestDetectStatePermissionDialogVariants covers the several phrasings
// we've observed Claude using for tool-permission prompts. If Claude
// adds a new variant, adding it here first reproduces the regression.
func TestDetectStatePermissionDialogVariants(t *testing.T) {
	cases := []string{
		"╭─\n│ Do you want to create demo.md?\n│ ❯ 1. Yes\n│   2. No\n╰─",
		"╭─\n│ Do you want to proceed?\n│ ❯ 1. Yes\n╰─",
		"╭─\n│ Do you want to run Bash(rm -rf /)?\n│ ❯ 1. Yes\n╰─",
		"╭─\n│ Claude requested permissions to edit /foo which is a sensitive file.\n│ ❯ 1. Yes\n│   2. No\n╰─",
		"╭─\n│ ❯ 1. Yes\n│   2. Yes, allow all edits during this session (shift+tab)\n│   3. No\n╰─",
	}
	for i, c := range cases {
		if got := detectState(c); got != StatePermission {
			t.Errorf("case %d: want permission-dialog, got %s\n%s", i, got, c)
		}
	}
}

// --- status on a non-claude pane (should be starting, not ready) ----------

func TestStatusNonClaude(t *testing.T) {
	sess := fmt.Sprintf("camux-shell-%d", time.Now().UnixNano())
	if out, err := exec.Command(amuxPath, "new", sess).CombinedOutput(); err != nil {
		t.Fatalf("amux new: %v %s", err, out)
	}
	t.Cleanup(func() { killAmuxSession(sess) })

	r := runCamux(t, "", "status", sess+":0")
	// A plain shell prompt won't match any Claude-specific markers, so
	// detectState returns "starting" — which camux maps to non-zero exit.
	out := strings.TrimSpace(r.stdout)
	if out != "starting" {
		t.Fatalf("want starting on plain shell, got %q (exit=%d stderr=%s)", out, r.exit, r.stderr)
	}
}

func TestStatusNotFound(t *testing.T) {
	r := runCamux(t, "", "status", "absolutely-no-such-session-xyz")
	if r.exit != 2 {
		t.Fatalf("want exit 2 for not-found, got %d (stdout=%q)", r.exit, r.stdout)
	}
	if strings.TrimSpace(r.stdout) != "not-found" {
		t.Fatalf("want 'not-found' stdout, got %q", r.stdout)
	}
}

// TestWaitOnReadyShell: wait against a non-Claude pane that's already
// settled. It will stay in "starting" state, so wait must time out —
// that's the correct behavior (wait waits for Ready, not "any state").
func TestWaitTimesOutOnNonClaude(t *testing.T) {
	sess := fmt.Sprintf("camux-wait-%d", time.Now().UnixNano())
	if out, err := exec.Command(amuxPath, "new", sess).CombinedOutput(); err != nil {
		t.Fatalf("amux new: %v %s", err, out)
	}
	t.Cleanup(func() { killAmuxSession(sess) })

	r := runCamux(t, "", "wait", sess+":0", "--timeout", "600ms", "--interval", "100ms")
	if r.exit == 0 {
		t.Fatalf("wait should not succeed on a non-claude pane (state=starting). stdout=%q", r.stdout)
	}
}

// TestHelpPerCommand proves `camux <cmd> -h` prints the long help.
func TestHelpPerCommand(t *testing.T) {
	r := runCamux(t, "", "spawn", "-h")
	if r.exit != 0 {
		t.Fatalf("spawn -h should exit 0, got %d (%s)", r.exit, r.stderr)
	}
	if !strings.Contains(r.stdout, "Launch Claude Code") {
		t.Fatalf("spawn -h missing expected text:\n%s", r.stdout)
	}
	// Also test `camux help <cmd>`.
	r = runCamux(t, "", "help", "ask")
	if r.exit != 0 {
		t.Fatalf("help ask should exit 0, got %d", r.exit)
	}
	if !strings.Contains(r.stdout, "Refuse unless") {
		t.Fatalf("help ask missing expected text:\n%s", r.stdout)
	}
}

// TestUnknownCommand proves unknown commands fail cleanly.
func TestUnknownCommand(t *testing.T) {
	r := runCamux(t, "", "flugelhorn")
	if r.exit != 2 {
		t.Fatalf("unknown command should exit 2, got %d", r.exit)
	}
	if !strings.Contains(r.stderr, "unknown command") {
		t.Fatalf("expected 'unknown command' in stderr, got: %s", r.stderr)
	}
}

// --- full e2e against real Claude (slow, gated) ---------------------------

func TestSpawnAsk(t *testing.T) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not on PATH")
	}
	_ = claudePath
	if os.Getenv("CAMUX_SKIP_CLAUDE") == "1" {
		t.Skip("CAMUX_SKIP_CLAUDE=1")
	}
	sess := fmt.Sprintf("camux-e2e-%d", time.Now().UnixNano())
	t.Cleanup(func() { killAmuxSession(sess) })

	// spawn
	r := runCamux(t, "", "spawn", sess, "--name", "cc", "--timeout", "60s")
	if r.exit != 0 {
		t.Fatalf("spawn exit=%d\nstderr=%s", r.exit, r.stderr)
	}
	target := strings.TrimSpace(r.stdout)
	if target != sess+":cc" {
		t.Fatalf("want target %s, got %q", sess+":cc", target)
	}

	// status
	r = runCamux(t, "", "status", target)
	if r.exit != 0 || strings.TrimSpace(r.stdout) != "ready" {
		t.Fatalf("status after spawn: exit=%d stdout=%q", r.exit, r.stdout)
	}

	// ask
	r = runCamux(t, "Reply with exactly: CAMUXE2E-MARKER-QQ\n",
		"ask", target, "--timeout", "90s")
	if r.exit != 0 {
		t.Fatalf("ask exit=%d\nstderr=%s\nstdout=%s", r.exit, r.stderr, r.stdout)
	}
	if !strings.Contains(r.stdout, "CAMUXE2E-MARKER-QQ") {
		t.Fatalf("ask output missing marker:\n%s", r.stdout)
	}
}

// --- trust / interrupt / clear on a real Claude ---------------------------

func TestInterruptClear(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not on PATH")
	}
	if os.Getenv("CAMUX_SKIP_CLAUDE") == "1" {
		t.Skip("CAMUX_SKIP_CLAUDE=1")
	}
	sess := fmt.Sprintf("camux-int-%d", time.Now().UnixNano())
	t.Cleanup(func() { killAmuxSession(sess) })

	r := runCamux(t, "", "spawn", sess, "--name", "cc", "--timeout", "60s")
	if r.exit != 0 {
		t.Fatalf("spawn: %s", r.stderr)
	}
	target := strings.TrimSpace(r.stdout)

	// Kick off a long reply.
	pasteCmd := exec.Command(amuxPath, "paste", target, "--submit")
	pasteCmd.Stdin = strings.NewReader("Count slowly from 1 to 500, one per line")
	if err := pasteCmd.Run(); err != nil {
		t.Fatalf("paste: %v", err)
	}
	// Wait a moment so it's definitely streaming.
	time.Sleep(3 * time.Second)

	r = runCamux(t, "", "status", target)
	if strings.TrimSpace(r.stdout) != "streaming" {
		t.Fatalf("expected streaming, got %q", r.stdout)
	}

	r = runCamux(t, "", "interrupt", target)
	if r.exit != 0 {
		t.Fatalf("interrupt failed: %s", r.stderr)
	}
	time.Sleep(1500 * time.Millisecond)
	r = runCamux(t, "", "status", target)
	if strings.TrimSpace(r.stdout) != "ready" {
		t.Fatalf("expected ready after interrupt, got %q", r.stdout)
	}

	// clear should be a no-op on empty input (always safe).
	r = runCamux(t, "", "clear", target)
	if r.exit != 0 {
		t.Fatalf("clear failed: %s", r.stderr)
	}
}
