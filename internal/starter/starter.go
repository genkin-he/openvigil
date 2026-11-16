// Package starter holds the tasks every new install begins with: only the self-evolution task, which
// improves whatever tasks the owner adds later from his feedback. Nobody's own tasks ship with Vigil.
package starter

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"vigil/internal/store"
	"vigil/internal/task"
)

//go:embed self-evolve.yaml
var selfEvolveYAML []byte

//go:embed self-evolve.md
var selfEvolveBrief string

// seededMark records that the starter tasks were offered once, so removing them is respected.
const seededMark = ".seeded"

// Seed adds the starter tasks to an install that has never had them. It does nothing on an install that
// was seeded before (even if the owner deleted them since) or that already has tasks of its own.
func Seed(st *store.Store) (bool, error) {
	mark := filepath.Join(st.Root(), seededMark)
	if _, err := os.Stat(mark); err == nil {
		return false, nil
	}
	tasks, _ := st.List()
	seeded := false
	if len(tasks) == 0 {
		t, err := task.Parse(selfEvolveYAML)
		if err != nil {
			return false, fmt.Errorf("starter task: %w", err)
		}
		if err := st.Put(t); err != nil {
			return false, fmt.Errorf("add starter task: %w", err)
		}
		if err := st.WriteDoc(t.ID, store.DocBrief, selfEvolveBrief); err != nil {
			return false, fmt.Errorf("write starter brief: %w", err)
		}
		seeded = true
	}
	if err := os.WriteFile(mark, nil, 0o600); err != nil && !errors.Is(err, os.ErrExist) {
		return seeded, fmt.Errorf("mark seeded: %w", err)
	}
	return seeded, nil
}
