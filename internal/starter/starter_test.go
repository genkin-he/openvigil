package starter

import (
	"regexp"
	"strings"
	"testing"

	"vigil/internal/store"
	"vigil/internal/task"
)

func TestSeedAddsSelfEvolveOnceToAnEmptyInstall(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := Seed(st); err != nil || !ok {
		t.Fatalf("seed = %v, %v", ok, err)
	}
	tk, err := st.Get("self-evolve")
	if err != nil || !tk.Enabled {
		t.Fatalf("task = %+v, %v", tk, err)
	}
	brief, _ := st.ReadDoc("self-evolve", store.DocBrief)
	if !strings.Contains(brief, "evolve signals") {
		t.Fatalf("brief = %.80q", brief)
	}
	// Removed by the owner: not brought back.
	if err := st.Remove("self-evolve"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := Seed(st); ok {
		t.Fatal("seeded twice")
	}
}

func TestSeedLeavesAnInstallWithTasksAlone(t *testing.T) {
	st, _ := store.New(t.TempDir())
	if err := st.Put(task.WithDefaults(task.Task{ID: "mine", Name: "mine", Trigger: task.Trigger{Kind: task.TriggerManual}})); err != nil {
		t.Fatal(err)
	}
	if ok, _ := Seed(st); ok {
		t.Fatal("seeded over existing tasks")
	}
	if _, err := st.Get("self-evolve"); err == nil {
		t.Fatal("starter task added")
	}
}

func TestStarterBriefIsGeneric(t *testing.T) {
	// The starter task ships to everyone: it may name only Vigil's own paths, never someone's tasks or files.
	text := selfEvolveBrief + string(selfEvolveYAML)
	if m := regexp.MustCompile(`~/[^.\s]|~/\.[^v]`).FindString(text); m != "" {
		t.Errorf("starter task points outside ~/.vigil: %q", m)
	}
	if strings.Count(text, "self-evolve") == 0 {
		t.Error("starter task lost its id")
	}
}
