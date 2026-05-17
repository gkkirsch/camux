package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Opt-in structured tracer for spawn-path debugging. Activated by setting
// CAMUX_SPAWN_TRACE to a writable file path. When unset, every traceLog
// call is a no-op — zero overhead in normal use.
//
// Format: tab-separated key=value pairs, one line per event. Values
// containing whitespace or quotes are Go-quoted. Designed for `grep`
// and `awk`, not for a log viewer.
//
// Why a separate file rather than stderr: stderr from camux gets
// captured by roster into a stringbuilder, which gets captured by
// director-app into setup.log. That's a synchronous, sequential write
// path — fine for a few lines of error context, bad for per-poll trace
// noise. The trace file is direct and unbuffered.

var (
	traceFile     *os.File
	traceMu       sync.Mutex
	traceInitOnce sync.Once
)

func traceInit() {
	traceInitOnce.Do(func() {
		path := os.Getenv("CAMUX_SPAWN_TRACE")
		if path == "" {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "camux: trace open %s: %v\n", path, err)
			return
		}
		traceFile = f
		_, _ = fmt.Fprintf(f, "\n=== camux %d %s ===\n",
			os.Getpid(), time.Now().Format(time.RFC3339Nano))
	})
}

// traceLog appends a structured event. kv must be alternating
// key/value strings. Best-effort: a write error is silently swallowed
// (we don't want trace noise to bubble up into normal flow).
func traceLog(category string, kv ...string) {
	traceInit()
	if traceFile == nil {
		return
	}
	var sb strings.Builder
	sb.WriteString(time.Now().Format("15:04:05.000"))
	sb.WriteByte('\t')
	sb.WriteString(category)
	for i := 0; i+1 < len(kv); i += 2 {
		sb.WriteByte('\t')
		sb.WriteString(kv[i])
		sb.WriteByte('=')
		sb.WriteString(traceQuote(kv[i+1]))
	}
	sb.WriteByte('\n')
	traceMu.Lock()
	_, _ = traceFile.WriteString(sb.String())
	traceMu.Unlock()
}

func traceQuote(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\n\"\\") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// snapshotTmux logs the current state of a session's windows + key
// window-options. Called at every transition we care about — before
// window creation, after window creation, before a NotFound bail-out.
// Best-effort: tmux errors are logged but don't disrupt the caller.
func snapshotTmux(category, session string) {
	if traceFile == nil {
		return
	}
	out, err := exec.Command("tmux", "list-windows", "-t", session, "-F",
		"#{window_index}:#{window_name}:active=#{window_active}:dead=#{pane_dead}:pid=#{pane_pid}").CombinedOutput()
	if err != nil {
		traceLog(category+".list-windows.error",
			"session", session,
			"err", err.Error(),
			"out", strings.TrimSpace(string(out)))
		return
	}
	traceLog(category+".list-windows",
		"session", session,
		"windows", strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", " | "))

	// Capture the global auto-rename + allow-rename settings — likely
	// culprit for the cc-window-gets-renamed-mid-spawn theory.
	opts, err := exec.Command("tmux", "show-options", "-g",
		"automatic-rename", "allow-rename").CombinedOutput()
	if err == nil {
		traceLog(category+".global-opts",
			"opts", strings.ReplaceAll(strings.TrimSpace(string(opts)), "\n", " | "))
	}
}

// snapshotWindow logs the live attributes of one window — including
// the resolved name (which differs from the requested name when
// automatic-rename has kicked in).
func snapshotWindow(category, target string) {
	if traceFile == nil {
		return
	}
	out, err := exec.Command("tmux", "display-message", "-p", "-t", target,
		"name=#{window_name} idx=#{window_index} active=#{window_active} dead=#{pane_dead} pid=#{pane_pid} cmd=#{pane_current_command}").CombinedOutput()
	if err != nil {
		traceLog(category+".display-message.error",
			"target", target, "err", err.Error(),
			"out", strings.TrimSpace(string(out)))
		return
	}
	traceLog(category+".display-message",
		"target", target,
		"resolved", strings.TrimSpace(string(out)))
}
