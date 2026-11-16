// Package notify tells the owner about runs that need attention.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	osexec "os/exec"
	"strings"
	"time"
)

const runTimeout = 30 * time.Second

// Message is one notification.
type Message struct {
	TaskID  string
	Title   string
	Status  string
	Summary string
	Detail  string // question, options, error
	Link    string
}

// Notifier delivers messages. A task's notify.command, when set, runs through bash with the message in
// VIGIL_* variables; otherwise a short text goes to the webhook when one is configured, and failing both a
// macOS notification is shown. Answers are given in the Vigil console.
type Notifier struct {
	Webhook string // optional: POST {"text": …} (WeChat / Slack / Feishu bots accept this shape)
	Log     func(format string, args ...any)
	// Desktop shows a local notification when no other channel is set (optional). There is no default:
	// a notification posted by osascript opens Script Editor when clicked, so the console's browser
	// notifications, which open the task, are the desktop channel.
	Desktop func(ctx context.Context, title, body string) error
}

// Send delivers m, using custom (a task's notify.command) when non-empty.
func (n Notifier) Send(ctx context.Context, m Message, custom string) error {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	var cmd *osexec.Cmd
	switch {
	case strings.TrimSpace(custom) != "":
		cmd = osexec.CommandContext(ctx, "/bin/bash", "-c", custom)
	case n.Webhook != "":
		n.webhook(ctx, m)
		return nil
	default:
		desktop := n.Desktop
		if desktop == nil {
			desktop = desktopDefault
		}
		if desktop == nil {
			return nil
		}
		if err := desktop(ctx, m.Title, m.Summary); err != nil {
			n.logf("notify %s: desktop: %v", m.TaskID, err)
		}
		return nil
	}
	cmd.Env = append(os.Environ(),
		"VIGIL_TASK="+m.TaskID, "VIGIL_TITLE="+m.Title, "VIGIL_STATUS="+m.Status,
		"VIGIL_SUMMARY="+m.Summary, "VIGIL_DETAIL="+m.Detail, "VIGIL_LINK="+m.Link)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("notify %s: %w: %s", m.TaskID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// desktopDefault is the desktop notifier used when none is set: none.
var desktopDefault func(ctx context.Context, title, body string) error

// webhook pushes a short text: no links and no raw content, just what happened and what is asked.
func (n Notifier) webhook(ctx context.Context, m Message) {
	if n.Webhook == "" {
		return
	}
	text := "【" + m.Title + "】" + m.Summary
	if m.Detail != "" {
		text += "\n" + m.Detail
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Webhook, bytes.NewReader(body))
	if err != nil {
		n.logf("webhook: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		n.logf("webhook: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		n.logf("webhook: status %s", resp.Status)
	}
}

func (n Notifier) logf(format string, args ...any) {
	if n.Log != nil {
		n.Log(format, args...)
	}
}

// ShouldNotifyFailure limits failure notifications: the 1st and 3rd in a row, then every 10th.
func ShouldNotifyFailure(streak int) bool {
	return streak == 1 || streak == 3 || (streak > 3 && streak%10 == 0)
}
