package store

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestOldRoutineMemoryMovesToArchiveRulesStay(t *testing.T) {
	s := newStore(t)
	_ = s.Put(manualTask("a"))
	path, _ := s.taskFile("a", DocMemory)
	old := strings.Join([]string{
		"# 长期记忆",
		"- 2026-11-01 决策：问进度类直接回",
		"- 2026-11-01 纠错：群聊没 @ 我不要回",
		"- 2026-11-01 我已授权：「问接口」类直接回",
		"- 2026-11-02 某线程已回复，等对方确认",
		"  续行属于上一条",
		"- 2026-11-02 feed-service 缓存 TTL 1 小时",
		"- 没有日期的一条",
		"- 2026-11-15 最近的流水",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.AppendMemory("a", []string{"今天的新流水"}, time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	mem, _ := s.ReadDoc("a", DocMemory)
	for _, keep := range []string{"# 长期记忆", "决策：问进度类直接回", "纠错：群聊没 @ 我", "我已授权：「问接口」", "没有日期的一条", "最近的流水", "今天的新流水"} {
		if !strings.Contains(mem, keep) {
			t.Errorf("memory lost %q:\n%s", keep, mem)
		}
	}
	for _, gone := range []string{"某线程已回复", "续行属于上一条", "缓存 TTL"} {
		if strings.Contains(mem, gone) {
			t.Errorf("memory still has old routine %q", gone)
		}
	}
	if !strings.Contains(mem, ArchiveFile) {
		t.Errorf("memory should point to the archive:\n%s", mem)
	}
	arch, err := os.ReadFile(strings.TrimSuffix(path, DocMemory) + ArchiveFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "- 2026-11-02 某线程已回复，等对方确认\n  续行属于上一条\n- 2026-11-02 feed-service 缓存 TTL 1 小时\n"
	if string(arch) != want {
		t.Fatalf("archive = %q", arch)
	}

	// A second pass moves nothing and keeps a single pointer line.
	if err := s.AppendMemory("a", []string{"再一条"}, time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	mem2, _ := s.ReadDoc("a", DocMemory)
	if strings.Count(mem2, ArchiveFile) != 1 {
		t.Fatalf("pointer repeated:\n%s", mem2)
	}
	if arch2, _ := os.ReadFile(strings.TrimSuffix(path, DocMemory) + ArchiveFile); string(arch2) != want {
		t.Fatalf("archive changed on a pass with nothing old: %q", arch2)
	}
}
