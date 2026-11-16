package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOwnerPrinciplesAPI(t *testing.T) {
	s, h := newServer(t)
	if rec, _ := do(h, "POST", "/api/owner/principles", `{"text":"  "}`, true); rec.Code != 400 {
		t.Fatalf("empty principle accepted: %d", rec.Code)
	}
	if rec, _ := do(h, "POST", "/api/owner/principles", `{"text":"代回不替我许诺时间 〔依据 3 次〕"}`, true); rec.Code != 200 {
		t.Fatalf("append: %d", rec.Code)
	}
	_, env := do(h, "GET", "/api/owner", "", true)
	var got ownerDoc
	raw, _ := json.Marshal(env.Data)
	json.Unmarshal(raw, &got)
	if !strings.Contains(got.Content, "代回不替我许诺时间") || got.Path != s.Store.OwnerPath() {
		t.Fatalf("owner = %+v", got)
	}
	if rec, _ := do(h, "PUT", "/api/owner", `{"content":"- 整理后的一份\n"}`, true); rec.Code != 200 {
		t.Fatalf("replace: %d", rec.Code)
	}
	if now, _ := s.Store.ReadOwner(); now != "- 整理后的一份\n" {
		t.Fatalf("after replace = %q", now)
	}
}
