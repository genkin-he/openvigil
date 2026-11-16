package replay

import "testing"

func TestCompare(t *testing.T) {
	reply := func(s string) Decision { return Decision{Action: ActionReply, Text: s, Valid: true} }
	act := func(a Action) Decision { return Decision{Action: a, Valid: true} }
	bad := Decision{}
	cases := []struct {
		name                  string
		before, after, actual Decision
		want                  Verdict
	}{
		{"same action", act(ActionIgnore), act(ActionIgnore), act(ActionTodo), Same},
		{"similar replies", reply("今天发 canary"), reply("今天发 canary。"), bad, Same},
		{"closer", act(ActionTodo), reply("已经修好了"), reply("已修好了"), Closer},
		{"farther", reply("已经修好了"), act(ActionTodo), reply("已修好了"), Farther},
		{"changed without actual", act(ActionTodo), act(ActionIgnore), bad, Changed},
		{"unknown", bad, act(ActionIgnore), act(ActionIgnore), Unknown},
	}
	for _, c := range cases {
		if got := Compare(c.before, c.after, c.actual); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestSimilarity(t *testing.T) {
	if Similarity("一样的话", "一样的话") != 1 {
		t.Fatal("identical must be 1")
	}
	if Similarity("abc", "xyz") != 0 {
		t.Fatal("disjoint must be 0")
	}
	if got := Similarity("已经修好了", "已修好"); got < 0.4 {
		t.Fatalf("near paraphrase = %v", got)
	}
}
