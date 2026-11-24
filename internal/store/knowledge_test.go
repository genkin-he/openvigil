package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryNotesSplitIntoRulesAndKnowledge(t *testing.T) {
	s := newStore(t)
	_ = s.Put(manualTask("a"))
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)

	notes := []string{
		"纠错：群聊没 @ 我不要回",
		"决策：问进度类直接回",
		"我已授权：「问接口」类直接回",
		"[user-service] 勋章表 user_flags，granted_at 非空才算已获得",
		"[User Service] 发帖走 CreatePostV2",
		"[../../etc] 主题名里的路径被清掉",
		"demo-cli 查日志没有 --since，用 --start",
		"2026-11-18 [monitor] 日志查询要带集群",
	}
	if err := s.AppendMemory("a", notes, at); err != nil {
		t.Fatal(err)
	}
	// The same fact again, on a later day, is not written twice.
	if err := s.AppendMemory("a", []string{"[user-service] 勋章表 user_flags，granted_at 非空才算已获得"}, at.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}

	mem, _ := s.ReadDoc("a", DocMemory)
	for _, want := range []string{"纠错：群聊没 @ 我", "决策：问进度类直接回", "我已授权：「问接口」"} {
		if !strings.Contains(mem, want) {
			t.Errorf("rule missing from memory.md: %q\n%s", want, mem)
		}
	}
	for _, gone := range []string{"user_flags", "monitor", "CreatePostV2"} {
		if strings.Contains(mem, gone) {
			t.Errorf("knowledge %q stayed in memory.md:\n%s", gone, mem)
		}
	}

	topics, err := s.Knowledge("a")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, k := range topics {
		got[k.Topic] = k.Entries
	}
	want := map[string]int{"user-service": 2, "monitor": 1, "misc": 1, "etc": 1}
	for topic, n := range want {
		if got[topic] != n {
			t.Errorf("topic %s = %d entries, want %d (all: %v)", topic, got[topic], n, got)
		}
	}
	dir, _ := s.TaskDir("a")
	body, _ := os.ReadFile(filepath.Join(dir, KnowledgeDir, "monitor.md"))
	if !strings.Contains(string(body), "- 2026-11-18 日志查询要带集群") {
		t.Errorf("monitor.md = %q", body)
	}
}

func TestRulesFromTheOwnerAlwaysStayInMemory(t *testing.T) {
	s := newStore(t)
	_ = s.Put(manualTask("a"))
	// An accepted rule the owner reworded: no rule keyword, still a rule.
	if err := s.AppendRules("a", []string{"「问接口」类直接以我的口吻回"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	mem, _ := s.ReadDoc("a", DocMemory)
	if !strings.Contains(mem, "「问接口」类直接以我的口吻回") {
		t.Fatalf("memory.md = %q", mem)
	}
	if topics, _ := s.Knowledge("a"); len(topics) != 0 {
		t.Fatalf("rule went to knowledge: %v", topics)
	}
}

func TestMemorySizeReportsRulesOnly(t *testing.T) {
	s := newStore(t)
	_ = s.Put(manualTask("a"))
	_ = s.AppendMemory("a", []string{"纠错：" + strings.Repeat("长", 100), "[x] " + strings.Repeat("知", 1000)}, time.Now())
	n, err := s.MemorySize("a")
	if err != nil {
		t.Fatal(err)
	}
	if n < 300 || n > 400 {
		t.Fatalf("memory size = %d, want only the rule line", n)
	}
}

func TestReadKnowledgeOnlyReadsTopicFiles(t *testing.T) {
	s := newStore(t)
	_ = s.Put(manualTask("a"))
	_ = s.AppendMemory("a", []string{"[monitor] 查日志要带集群"}, time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC))
	got, err := s.ReadKnowledge("a", "monitor")
	if err != nil || !strings.Contains(got, "查日志要带集群") {
		t.Fatalf("read = %q, %v", got, err)
	}
	for _, bad := range []string{"../memory", "../../token", "Monitor", "", "nope"} {
		if _, err := s.ReadKnowledge("a", bad); err == nil {
			t.Errorf("topic %q was read", bad)
		}
	}
}
