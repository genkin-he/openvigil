// Package slackctl posts and reads Slack messages as the owner through the slackctl CLI.
package slackctl

import (
	"context"
	"encoding/json"
	"fmt"
	osexec "os/exec"
	"strconv"
	"strings"
	"time"
)

const callTimeout = time.Minute

// Client calls slackctl.
type Client struct {
	Bin string // default "slackctl"
}

func (c Client) run(ctx context.Context, args ...string) ([]byte, error) {
	bin := c.Bin
	if bin == "" {
		bin = "slackctl"
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	out, err := osexec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*osexec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("slackctl %s: %w %s", args[0]+" "+args[1], err, stderr)
	}
	return out, nil
}

// Post sends text to channel (in thread when set) and returns the new message's ts.
func (c Client) Post(ctx context.Context, channel, text, thread string) (string, error) {
	args := []string{"message", "send", channel, text}
	if thread != "" {
		args = append(args, "--thread-ts", thread)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return parseTS(out)
}

// Message is one message of a thread.
type Message struct {
	TS   string `json:"ts"`
	User string `json:"user"`
	Text string `json:"text"`
}

// Messages lists a thread, parent included, oldest first.
func (c Client) Messages(ctx context.Context, channel, thread string) ([]Message, error) {
	out, err := c.run(ctx, "message", "list", channel, "--thread-ts", thread, "--limit", "200")
	if err != nil {
		return nil, err
	}
	return decodeMessages(out)
}

// History lists a channel's latest messages (top level only), newest first.
func (c Client) History(ctx context.Context, channel string, limit int) ([]Message, error) {
	out, err := c.run(ctx, "message", "list", channel, "--limit", strconv.Itoa(limit))
	if err != nil {
		return nil, err
	}
	return decodeMessages(out)
}

func decodeMessages(out []byte) ([]Message, error) {
	var msgs []Message
	if err := json.Unmarshal(out, &msgs); err != nil {
		return nil, fmt.Errorf("decode thread: %w", err)
	}
	return msgs, nil
}

// Thread returns the texts of a thread, parent included.
func (c Client) Thread(ctx context.Context, channel, ts string) ([]string, error) {
	msgs, err := c.Messages(ctx, channel, ts)
	if err != nil {
		return nil, err
	}
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		texts = append(texts, m.Text)
	}
	return texts, nil
}

// SelfID returns the Slack user id slackctl acts as.
func (c Client) SelfID(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "auth", "test")
	if err != nil {
		return "", err
	}
	var v struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.UserID == "" {
		return "", fmt.Errorf("auth test has no user_id: %.200s", out)
	}
	return v.UserID, nil
}

func parseTS(out []byte) (string, error) {
	var v struct {
		TS      string `json:"ts"`
		Message struct {
			TS string `json:"ts"`
		} `json:"message"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", fmt.Errorf("decode send result: %w", err)
	}
	if ts := v.TS; ts != "" {
		return ts, nil
	}
	if ts := v.Message.TS; ts != "" {
		return ts, nil
	}
	return "", fmt.Errorf("send result has no ts: %.200s", out)
}

// Reply returns the full text of one reply in a thread. (`message get` on a reply returns the thread's
// parent, so the thread is listed and the reply picked by ts.)
func (c Client) Reply(ctx context.Context, channel, thread, ts string) (string, error) {
	msgs, err := c.Messages(ctx, channel, thread)
	if err != nil {
		return "", err
	}
	return pickReply(msgs, ts)
}

func pickReply(msgs []Message, ts string) (string, error) {
	for _, m := range msgs {
		if m.TS == ts {
			return m.Text, nil
		}
	}
	return "", fmt.Errorf("reply %s not in thread", ts)
}
