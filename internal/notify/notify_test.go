package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSendCustomCommandGetsEnv(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env")
	err := Notifier{}.Send(context.Background(), Message{TaskID: "a", Title: "T", Summary: "$(whoami)"}, `printf '%s' "$VIGIL_SUMMARY" > `+out)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(out); string(data) != "$(whoami)" {
		t.Fatalf("summary was interpreted: %q", data)
	}
}

func TestSendWithoutChannelShowsDesktopNotification(t *testing.T) {
	var got []string
	n := Notifier{Desktop: func(_ context.Context, title, body string) error {
		got = append(got, title, body)
		return nil
	}}
	if err := n.Send(context.Background(), Message{Title: "失败了", Summary: "连续 3 次"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "失败了" || got[1] != "连续 3 次" {
		t.Fatalf("desktop = %q (the title already names Vigil)", got)
	}
}

// A macOS notification posted by osascript opens Script Editor when clicked, so none is shown by
// default: the console's browser notifications, which open the task, take their place.
func TestNoDesktopNotificationByDefault(t *testing.T) {
	if err := (Notifier{}).Send(context.Background(), Message{Title: "失败了"}, ""); err != nil {
		t.Fatal(err)
	}
	if desktopDefault != nil {
		t.Fatal("a default desktop notifier is set")
	}
}

func TestShouldNotifyFailure(t *testing.T) {
	want := map[int]bool{1: true, 2: false, 3: true, 4: false, 10: true, 11: false, 20: true}
	for streak, w := range want {
		if ShouldNotifyFailure(streak) != w {
			t.Fatalf("streak %d", streak)
		}
	}
}

func TestSendPushesWebhook(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}))
	defer srv.Close()
	if err := (Notifier{Webhook: srv.URL}).Send(context.Background(), Message{TaskID: "a", Title: "T", Summary: "S"}, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "【T】S") {
		t.Fatalf("webhook body = %q", got)
	}
}
