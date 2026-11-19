package forge

import (
	"context"
	"strings"
	"testing"

	"vigil/internal/herdr"
)

// A Claude Code question with described options, as herdr reads it.
const questionScreen = `● 接下来要确定的只剩清理范围有多大。
────────────────────────────────────────────────────────────
 □ 清理范围

 每天自动清理时要覆盖哪些？

❯ 1. 所有默认勾选项 (Recommended)
     开发缓存 + Docker 构建缓存
  2. 只清开发缓存
     只清默认勾选的项
  3. Type something.
────────────────────────────────────────────────────────────
  4. Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel
`

func TestSayToAnAgentStoppedOnAQuestionPicksTheOption(t *testing.T) {
	for text, want := range map[string]string{"1": "1", " 2 ": "2", "只清开发缓存": "2", "所有默认勾选项": "1"} {
		f := &fakeHerdr{promptErr: &herdr.APIError{Code: herdr.CodeAgentBlocked}, screen: questionScreen}
		if err := (Forge{H: f}).Say(context.Background(), "forge-a1", text); err != nil {
			t.Fatalf("Say(%q): %v", text, err)
		}
		if len(f.keys) != 1 || strings.Join(f.keys[0], ",") != want {
			t.Fatalf("Say(%q) keys = %v, want %s", text, f.keys, want)
		}
	}
}

func TestSayFreeTextToAStoppedAgentListsTheOptions(t *testing.T) {
	f := &fakeHerdr{promptErr: &herdr.APIError{Code: herdr.CodeAgentBlocked}, screen: questionScreen}
	err := (Forge{H: f}).Say(context.Background(), "forge-a1", "随便清")
	if err == nil || !strings.Contains(err.Error(), "1. 所有默认勾选项") || !strings.Contains(err.Error(), "4. Chat about this") {
		t.Fatalf("err = %v", err)
	}
	if len(f.keys) != 0 {
		t.Fatalf("pressed %v", f.keys)
	}
}
