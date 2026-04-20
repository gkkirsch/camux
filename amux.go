package main

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// amuxBinName is the amux executable we delegate to. Override with AMUX_BIN.
var amuxBinName = "amux"

// runAmux calls `amux <args...>` and returns stdout. Non-zero exit becomes
// an error whose message embeds stderr.
func runAmux(args ...string) (string, error) {
	return runAmuxStdin(nil, args...)
}

func runAmuxStdin(stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command(amuxBinName, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("amux %s: %s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

// amuxExists returns true iff the amux `exists` check succeeds on target.
// Silent, no stderr on success; non-zero exit = does not exist.
func amuxExists(target string) bool {
	cmd := exec.Command(amuxBinName, "exists", target)
	return cmd.Run() == nil
}

// capture is a convenience around `amux capture`.
func capture(target string, lines int) (string, error) {
	args := []string{"capture", target}
	if lines > 0 {
		args = append(args, "--lines", fmt.Sprint(lines))
	}
	return runAmux(args...)
}
