package main

import (
	"strings"
	"testing"
)

func TestRequireHerdr(t *testing.T) {
	if err := requireHerdr("sh"); err != nil {
		t.Fatalf("an installed tool was refused: %v", err)
	}
	err := requireHerdr("/nonexistent/herdr")
	if err == nil || !strings.Contains(err.Error(), "herdr.dev/install.sh") {
		t.Fatalf("missing herdr: %v", err)
	}
}
