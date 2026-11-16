package terminal

import "testing"

var table = []Proc{
	{PID: 100, PPID: 1, Args: "/Applications/Ghostty.app/Contents/MacOS/ghostty"},
	{PID: 101, PPID: 100, Args: "/usr/bin/login -flp alice"},
	{PID: 102, PPID: 101, Args: "-/bin/zsh"},
	{PID: 103, PPID: 102, Args: "herdr"},
	{PID: 200, PPID: 1, Args: "/opt/tools/bin/herdr server"},
	{PID: 300, PPID: 1, Args: "/Applications/iTerm.app/Contents/MacOS/iTerm2"},
	{PID: 301, PPID: 300, Args: "-zsh"},
	{PID: 302, PPID: 301, Args: "/opt/tools/bin/herdr --session work"},
	{PID: 400, PPID: 103, Args: "/opt/tools/bin/herdr --session vigil agent attach v-x"},
}

func TestFindApp(t *testing.T) {
	cases := map[string]string{
		"default": "/Applications/Ghostty.app",
		"":        "/Applications/Ghostty.app",
		"work":    "/Applications/iTerm.app",
	}
	for session, want := range cases {
		got, err := FindApp(table, session)
		if err != nil || got != want {
			t.Fatalf("FindApp(%q) = %q, %v", session, got, err)
		}
	}
	if _, err := FindApp(table, "missing"); err == nil {
		t.Fatal("expected an error when no client is attached")
	}
}

func TestServerAndAttachAreNotClients(t *testing.T) {
	for _, args := range []string{"/x/herdr server", "/x/herdr --session vigil agent attach v-x", "/x/notherdr"} {
		if isClient(args, "default") || isClient(args, "vigil") {
			t.Fatalf("%q treated as a UI client", args)
		}
	}
}
