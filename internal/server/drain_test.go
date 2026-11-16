package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestDrainEndpoints(t *testing.T) {
	s, h := newServer(t)
	rec, _ := do(h, "POST", "/api/drain", `{"on":true}`, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"draining":true`) {
		t.Fatalf("drain on: %d %s", rec.Code, rec.Body)
	}
	if !s.Engine.Draining() {
		t.Fatal("engine not draining")
	}
	if rec, _ := do(h, "GET", "/api/overview", "", true); !strings.Contains(rec.Body.String(), `"draining":true`) {
		t.Fatalf("overview misses draining: %s", rec.Body)
	}
	rec, _ = do(h, "POST", "/api/drain", `{"on":false}`, true)
	if rec.Code != http.StatusOK || s.Engine.Draining() {
		t.Fatalf("drain off: %d %s", rec.Code, rec.Body)
	}
}
