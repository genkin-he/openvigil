// Package replay asks an agent, with no tools at all, what it would have done in a past round under a
// given set of rules. It compares decisions, never re-executes anything: the model sees the round's input
// and the evidence recorded then, and answers in a fixed shape.
package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"time"
)

// Action is what a round does about its input.
type Action string

const (
	ActionReply  Action = "reply"  // send a reply
	ActionTodo   Action = "todo"   // hand it to the owner
	ActionIgnore Action = "ignore" // nothing to do
	ActionReact  Action = "react"  // an emoji, no text
)

func (a Action) valid() bool {
	return a == ActionReply || a == ActionTodo || a == ActionIgnore || a == ActionReact
}

// Decision is the model's answer for one round.
type Decision struct {
	Action Action `json:"action"`
	Text   string `json:"text,omitempty"` // what would be sent, for a reply
	Why    string `json:"why,omitempty"`
	Valid  bool   `json:"valid"` // parsed, with a known action
}

// Case is one past round.
type Case struct {
	RunID    string
	Input    string // the round's input as it arrived
	Evidence string // what the round found out then, clipped; "" when nothing was recorded
}

const decisionSchema = `{"type":"object","properties":{"action":{"type":"string","enum":["reply","todo","ignore","react"]},` +
	`"text":{"type":"string"},"why":{"type":"string"}},"required":["action","why"]}`

// Args is the claude command line for one replay. It disables every built-in tool and every MCP server,
// skips settings (hooks, permissions) and leaves no session behind: a replay can only answer.
func Args(model string) []string {
	return []string{"-p", "--output-format", "json", "--model", model, "--tools", "",
		"--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence", "--setting-sources", "",
		"--json-schema", decisionSchema}
}

// Prompt asks for the decision on one round under the given rules.
func Prompt(rules string, c Case) string {
	evidence := strings.TrimSpace(c.Evidence)
	if evidence == "" {
		evidence = "（没有记录）"
	}
	return "下面是你作为值守 agent 要遵守的全部规则：\n\n" + rules +
		"\n\n---\n以下是当时这一轮收到的输入：\n\n" + c.Input +
		"\n\n---\n当时为这一轮查到的信息：\n\n" + evidence +
		"\n\n---\n按上面的规则，这一轮你会怎么处理？你现在没有任何工具，也不能再去查，只根据上面的信息判断。" +
		"action 选 reply（直接回复）、todo（交给我拍板）、ignore（不用处理）、react（只点表情）；" +
		"reply 时 text 写要发出的原文；why 一句话说明理由。"
}

// callTimeout bounds one replay call, so a hung claude cannot stall a replay.
const callTimeout = 3 * time.Minute

// Runner calls claude for replays.
type Runner struct {
	Bin   string
	Model string
	// Run executes the command in dir with stdin; nil runs it for real.
	Run func(ctx context.Context, dir string, args []string, stdin string) ([]byte, error)
}

// Decide replays one round under rules and returns the decision and what the call cost.
func (r Runner) Decide(ctx context.Context, rules string, c Case) (Decision, float64, error) {
	dir, err := os.MkdirTemp("", "vigil-replay-")
	if err != nil {
		return Decision{}, 0, fmt.Errorf("replay dir: %w", err)
	}
	defer os.RemoveAll(dir)
	run := r.Run
	if run == nil {
		run = r.exec
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	out, err := run(ctx, dir, Args(r.Model), Prompt(rules, c))
	if err != nil {
		return Decision{}, 0, fmt.Errorf("replay %s: %w", c.RunID, err)
	}
	d, cost := ParseDecision(out)
	return d, cost, nil
}

func (r Runner) exec(ctx context.Context, dir string, args []string, stdin string) ([]byte, error) {
	cmd := osexec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.Output()
}

// ParseDecision reads claude's json output: the structured answer, or the same JSON as the result text.
func ParseDecision(raw []byte) (Decision, float64) {
	var res struct {
		Result     string          `json:"result"`
		Structured json.RawMessage `json:"structured_output"`
		Cost       float64         `json:"total_cost_usd"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return Decision{}, 0
	}
	var d Decision
	body := res.Structured
	if len(body) == 0 || string(body) == "null" {
		body = json.RawMessage(res.Result)
	}
	if json.Unmarshal(body, &d) != nil || !d.Action.valid() {
		return Decision{}, res.Cost
	}
	d.Valid = true
	return d, res.Cost
}
