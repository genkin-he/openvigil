package main

import (
	"flag"
	"fmt"
)

// cmdRetro prints the facts of the last days as JSON, for the weekly review.
func cmdRetro(args []string) error {
	fs := flag.NewFlagSet("retro", flag.ContinueOnError)
	days := fs.Int("days", 7, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return getJSON(fmt.Sprintf("/api/retro?days=%d", *days))
}
