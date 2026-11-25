package inbox

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func newBox(t *testing.T) *Box {
	t.Helper()
	clock := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	b, err := Open(t.TempDir(), func() time.Time { clock = clock.Add(time.Minute); return clock })
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAddRequiresTitleAndTask(t *testing.T) {
	b := newBox(t)
	for _, in := range []Item{{TaskID: "slack-watch"}, {Title: "x"}} {
		if _, _, err := b.Add(in); err == nil {
			t.Fatalf("Add(%+v) should fail", in)
		}
	}
}

func TestAddSameRefUpdatesOpenItem(t *testing.T) {
	b := newBox(t)
	first, created, err := b.Add(Item{TaskID: "slack-watch", Title: "小林：要闻不对", Ref: "C1:1.2", Reply: "草稿"})
	if err != nil || !created {
		t.Fatalf("first add: created=%v err=%v", created, err)
	}
	second, created, err := b.Add(Item{TaskID: "slack-watch", Title: "小林：要闻不对", Ref: "C1:1.2", Analysis: "查到了"})
	if err != nil || created {
		t.Fatalf("second add should update: created=%v err=%v", created, err)
	}
	if second.ID != first.ID || second.Reply != "草稿" || second.Analysis != "查到了" {
		t.Fatalf("merged = %+v", second)
	}
	if items, _ := b.List(false); len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
}

func TestClosedItemDoesNotAbsorbNewReport(t *testing.T) {
	b := newBox(t)
	first, _, _ := b.Add(Item{TaskID: "t", Title: "a", Ref: "r"})
	if _, err := b.Close(first.ID, "已回复"); err != nil {
		t.Fatal(err)
	}
	again, created, err := b.Add(Item{TaskID: "t", Title: "a again", Ref: "r"})
	if err != nil || !created || again.ID == first.ID {
		t.Fatalf("a finished item must not be reused: created=%v id=%s err=%v", created, again.ID, err)
	}
}

func TestAnswerOnlyOnce(t *testing.T) {
	b := newBox(t)
	it, _, _ := b.Add(Item{TaskID: "t", Title: "a"})
	if _, err := b.Answer(it.ID, "  "); err == nil {
		t.Fatal("empty answer should fail")
	}
	got, err := b.Answer(it.ID, "A")
	if err != nil || got.Status != StatusAnswered || got.Answer != "A" {
		t.Fatalf("answer: %+v %v", got, err)
	}
	if _, err := b.Answer(it.ID, "B"); !errors.Is(err, ErrClosed) {
		t.Fatalf("second answer err = %v", err)
	}
}

func TestNoteReopenAndClose(t *testing.T) {
	b := newBox(t)
	it, _, _ := b.Add(Item{TaskID: "t", Title: "a", Analysis: "一"})
	_, _ = b.Answer(it.ID, "发")
	got, err := b.Note(it.ID, "发送失败：没权限", true)
	if err != nil || got.Status != StatusOpen || got.Analysis != "一\n发送失败：没权限" || got.Answer != "" {
		t.Fatalf("note+reopen: %+v %v", got, err)
	}
	done, err := b.Close(it.ID, "已发出")
	if err != nil || done.Status != StatusDone || done.Outcome != "已发出" {
		t.Fatalf("close: %+v %v", done, err)
	}
	if _, err := b.Note(it.ID, "x", false); !errors.Is(err, ErrClosed) {
		t.Fatalf("note on closed err = %v", err)
	}
	if open, _ := b.List(false); len(open) != 0 {
		t.Fatalf("closed items must leave the open list: %+v", open)
	}
	if all, _ := b.List(true); len(all) != 1 {
		t.Fatalf("all list = %d", len(all))
	}
}

func TestGetRejectsBadIDs(t *testing.T) {
	b := newBox(t)
	for _, id := range []string{"", "../etc/passwd", "td-zzzzzz", "td-123456"} {
		if _, err := b.Get(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(%q) err = %v", id, err)
		}
	}
}

func TestListNewestFirst(t *testing.T) {
	b := newBox(t)
	a, _, _ := b.Add(Item{TaskID: "t", Title: "old"})
	c, _, _ := b.Add(Item{TaskID: "t", Title: "new"})
	items, _ := b.List(false)
	if len(items) != 2 || items[0].ID != c.ID || items[1].ID != a.ID {
		t.Fatalf("order = %+v", items)
	}
}

func TestThirdHandBackTurnsItIntoTheOwnersStep(t *testing.T) {
	b := newBox(t)
	it, _, _ := b.Add(Item{TaskID: "self-evolve", Title: "放行 approve.py", Reply: "改 settings.json"})
	for i := 1; i <= MaxReopens; i++ {
		if _, err := b.Answer(it.ID, "照办"); err != nil {
			t.Fatal(err)
		}
		got, err := b.HandBack(it.ID, "又被拦了")
		if err != nil {
			t.Fatal(err)
		}
		if got.Reopens != i {
			t.Fatalf("reopens = %d, want %d", got.Reopens, i)
		}
		if got.Owner != (i == MaxReopens) {
			t.Fatalf("after %d hand-backs owner = %v", i, got.Owner)
		}
	}
	got, _ := b.Get(it.ID)
	if !strings.Contains(got.Analysis, "亲手") {
		t.Fatalf("analysis does not say why: %q", got.Analysis)
	}
	// Vigil reopening an answered card itself (a failed memory write) is not the task handing it back.
	card, _, _ := b.Add(Item{TaskID: "slack", Title: "收回授权"})
	b.Answer(card.ID, "是")
	if got, _ := b.Note(card.ID, "写记忆失败", true); got.Reopens != 0 || got.Status != StatusOpen {
		t.Fatalf("an internal reopen counted as a hand-back: %+v", got)
	}
	// A note on an open item (more analysis, no hand-back) is not counted.
	other, _, _ := b.Add(Item{TaskID: "slack", Title: "x"})
	if got, _ := b.HandBack(other.ID, "补充"); got.Reopens != 0 {
		t.Fatalf("an open item's note counted as a hand-back: %d", got.Reopens)
	}
}

func TestChoicesMustBeAFewShortDistinctLabels(t *testing.T) {
	ok := []Choice{{Label: "批这一次", Note: "只批这张，不改白名单"}, {Label: "不批"}}
	if err := ValidChoices(ok); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("长", 31)
	for name, cs := range map[string][]Choice{
		"one":       {{Label: "只有一个"}},
		"too many":  {{Label: "1"}, {Label: "2"}, {Label: "3"}, {Label: "4"}, {Label: "5"}, {Label: "6"}},
		"empty":     {{Label: " "}, {Label: "x"}},
		"duplicate": {{Label: "批"}, {Label: " 批 "}},
		"long":      {{Label: long}, {Label: "x"}},
		"digit":     {{Label: "2"}, {Label: "x"}}, // a bare number would clash with answering by number in Slack
	} {
		if err := ValidChoices(cs); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if ValidChoices(nil) != nil {
		t.Error("no choices is fine")
	}
}

func TestARefreshKeepsTheChoices(t *testing.T) {
	b, _ := Open(t.TempDir(), nil)
	first, _, _ := b.Add(Item{TaskID: "t", Title: "审批", Ref: "r", Choices: []Choice{{Label: "批"}, {Label: "不批"}}})
	again, created, _ := b.Add(Item{TaskID: "t", Title: "审批", Ref: "r", Analysis: "补充"})
	if created || again.ID != first.ID || len(again.Choices) != 2 {
		t.Fatalf("refresh = %+v", again)
	}
}

func TestOwnerCanStillAnswerAnItemItsTaskClosed(t *testing.T) {
	b := newBox(t)
	it, _, _ := b.Add(Item{TaskID: "t", Title: "a"})
	if _, err := b.CloseByTask(it.ID, "对方已拍板，转为排期待办"); err != nil {
		t.Fatal(err)
	}
	got, err := b.Answer(it.ID, "维持 a")
	if err != nil || got.Status != StatusAnswered || got.Answer != "维持 a" || got.TaskClosed {
		t.Fatalf("answer after task close: %+v %v", got, err)
	}
	if got.Outcome != "对方已拍板，转为排期待办" {
		t.Fatalf("the task's reason for closing must stay for the task to see: %q", got.Outcome)
	}
}

func TestOwnerClosedItemStaysClosed(t *testing.T) {
	b := newBox(t)
	it, _, _ := b.Add(Item{TaskID: "t", Title: "a"})
	if _, err := b.Close(it.ID, "不用管"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Answer(it.ID, "发"); !errors.Is(err, ErrClosed) {
		t.Fatalf("answer after owner close err = %v", err)
	}
}

// The run carrying the owner's answer failed: the item says so until the answer is handed over again.
func TestAFailedHandoffIsMarkedUntilTheAnswerGoesOutAgain(t *testing.T) {
	b := newBox(t)
	it, _, _ := b.Add(Item{TaskID: "slack", Title: "陶然：少了两笔期权单"})
	if _, err := b.HandoffFailed(it.ID, "API Error"); !errors.Is(err, ErrClosed) {
		t.Fatalf("an open item has no handoff to fail: %v", err)
	}
	if _, err := b.Answer(it.ID, "查清楚再回复"); err != nil {
		t.Fatal(err)
	}
	got, err := b.HandoffFailed(it.ID, "API Error: can't receive replies")
	if err != nil || got.HandoffError != "API Error: can't receive replies" || got.Status != StatusAnswered {
		t.Fatalf("got %+v, %v", got, err)
	}
	again, err := b.Resend(it.ID)
	if err != nil || again.HandoffError != "" || again.Status != StatusAnswered || again.Answer != "查清楚再回复" {
		t.Fatalf("resend: %+v, %v", again, err)
	}
	if _, err := b.Close(it.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resend(it.ID); !errors.Is(err, ErrClosed) {
		t.Fatalf("a closed item cannot be resent: %v", err)
	}
}
