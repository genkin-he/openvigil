package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"vigil/internal/resident"
)

// cmdGuard is the PreToolUse hook of resident sessions: it refuses a call the owner's ask rules would stop
// on, so the session never sits on a dialog. Anything it cannot read lets the call through to Claude Code's
// own checks, which still ask: the guard only ever refuses earlier, never allows.
func cmdGuard(args []string) error {
	if len(args) != 2 || args[0] != "--task" {
		return errors.New(commands["guard"].usage)
	}
	out, err := guardVerdict(os.Stdin, args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "vigil guard:", err)
		return nil
	}
	_, err = os.Stdout.Write(out)
	return err
}

func guardVerdict(stdin io.Reader, taskID string) ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}
	settings, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return nil, fmt.Errorf("read owner settings: %w", err)
	}
	rules, err := resident.AskRules(settings)
	if err != nil {
		return nil, err
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("read hook payload: %w", err)
	}
	out, _, err := resident.GuardVerdict(payload, rules, taskID)
	return out, err
}
