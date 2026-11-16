package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookPathFindsToolsInLocalBinOffPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := lookPath("herdr"); got != filepath.Join(bin, "herdr") {
		t.Fatalf("lookPath = %q", got)
	}
	if got := lookPath("nope"); got != "nope" {
		t.Fatalf("missing tool = %q, want the bare name", got)
	}
}

func TestProbesAreReadFromConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIGIL_HOME", dir)
	yml := "probes:\n  - name: ticket\n    match: '^ticket:\\d+$'\n    command: ~/bin/ticket-probe\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Probes) != 1 || c.Probes[0].Name != "ticket" || c.Probes[0].Command != "~/bin/ticket-probe" {
		t.Fatalf("probes = %+v", c.Probes)
	}
}
