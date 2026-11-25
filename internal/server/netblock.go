package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"vigil/internal/runner"
	"vigil/internal/store"
)

// A blocked network: Anthropic's edge refuses requests from the current proxy / VPN exit (403
// network_edge_blocked, or a Cloudflare challenge page). Every run fails the same way until the exit
// changes, so the failures become one system to-do instead of a pile of red runs, and the rounds that
// were lost are run again once a run gets through (or the owner says it is fixed).
const (
	netBlockedRef  = "net:blocked"
	netBlockedKeep = 20              // lost rounds remembered for a re-run; older ones are only counted
	netProbeEvery  = 3 * time.Minute // how often a blocked network is checked while nothing else runs
	netProbeURL    = "https://api.anthropic.com/v1/models"
	netRerunNote   = "（这一轮之前因为网络或账号被拦没跑完，现在重跑。先确认上次有没有已经做过的动作，别重复发消息、别重复建待办。）\n"
)

var netBlockedText = regexp.MustCompile(`network_edge_blocked|Request blocked before it reached Anthropic|Attention Required! \| Cloudflare`)

// A blocked account: Anthropic refuses the organization itself (11-25 12:33–13:51, 47 rounds failed,
// nothing raised, nothing re-run). It is handled like a blocked network, with its own to-do; the edge
// probe cannot see it end, so only a round getting through (or the owner's 复查) closes it.
const accountBlockedRef = "api:account"

var accountBlockedText = regexp.MustCompile(`can't receive replies from Claude|Anthropic Verification Portal`)

// netLost is a round that failed on the blocked network, kept to run again.
type netLost struct {
	task, input string
}

type netBlock struct {
	mu   sync.Mutex
	lost []netLost
	more int // lost rounds beyond netBlockedKeep
	// account is set while the block is the account's: the edge probe says nothing about that.
	account bool
	probe   func(context.Context) bool // nil: ask Anthropic's edge (tests replace it)
}

// NetWatchLoop follows finished runs for a blocked network until ctx ends.
func (s *Server) NetWatchLoop(ctx context.Context) {
	events, stop := s.Engine.Hub().Subscribe()
	defer stop()
	tick := time.NewTicker(netProbeEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.probeNet(ctx)
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Type == "run.finished" {
				s.noteRunForNet(ev)
			}
		}
	}
}

// noteRunForNet follows a finished run: a failed handoff of the owner's answer marks its to-do, and a
// blocked network or account keeps the lost round to run again.
func (s *Server) noteRunForNet(ev runner.Event) {
	rec, ok := s.runRecord(ev.TaskID, ev.RunID)
	if !ok || rec.Job != "" { // background jobs are not re-run: their parent decides
		return
	}
	s.noteHandoff(rec)
	nb := &s.net
	switch {
	case rec.Status == "ok" || rec.Status == "needs_you":
		nb.mu.Lock()
		blocked := len(nb.lost) > 0 || nb.more > 0
		nb.lost = without(nb.lost, netLost{rec.TaskID, rec.Input}) // got through on its own retry
		nb.mu.Unlock()
		if blocked {
			s.netRecovered("恢复了（" + rec.TaskID + " 这一轮跑通了）")
		}
	case accountBlockedText.MatchString(rec.Error):
		summary := nb.lose(netLost{rec.TaskID, rec.Input}, true)
		if err := s.raiseSystem(accountBlockedRef, "Claude 账号被拦：Anthropic 拒绝了这个组织的请求",
			"报错是「This organization can't receive replies from Claude right now」，要组织管理员在 Anthropic 的验证页面处理，"+
				"换网络没用。处理好后点「复查」立刻重跑丢掉的几轮；不点也行，任何一轮跑通后会自动重跑。", summary); err != nil {
			s.logf("account blocked: raise to-do: %v", err)
		}
	case netBlockedText.MatchString(rec.Error):
		summary := nb.lose(netLost{rec.TaskID, rec.Input}, false)
		if err := s.raiseSystem(netBlockedRef, "网络被拦：Claude 的请求被 Anthropic 挡下",
			"代理 / VPN 当前的出口地区或节点被拒（403 network_edge_blocked）。换一个支持的地区或节点，"+
				"然后点「复查」立刻重跑丢掉的几轮；不点也行，Vigil 每 3 分钟探一次，网络一通就自动重跑。", summary); err != nil {
			s.logf("net blocked: raise to-do: %v", err)
		}
	}
}

// lose keeps a round lost to a block for a re-run and returns the to-do's summary.
func (nb *netBlock) lose(l netLost, account bool) string {
	nb.mu.Lock()
	defer nb.mu.Unlock()
	if account {
		nb.account = true
	}
	if !contains(nb.lost, l) {
		if len(nb.lost) < netBlockedKeep {
			nb.lost = append(nb.lost, l)
		} else {
			nb.more++
		}
	}
	return nb.summaryLocked()
}

// probeNet checks a blocked network directly: hours can pass without any run that would show it
// is back (11-23 the morning brief waited 08:04→09:50 for the owner to click 复查).
func (s *Server) probeNet(ctx context.Context) {
	nb := &s.net
	nb.mu.Lock()
	blocked := (len(nb.lost) > 0 || nb.more > 0) && !nb.account
	probe := nb.probe
	nb.mu.Unlock()
	if !blocked {
		return
	}
	if probe == nil {
		probe = func(ctx context.Context) bool { return edgeReachable(ctx, netProbeURL) }
	}
	if probe(ctx) {
		s.netRecovered("网络恢复了（探测到 Anthropic 不再拦截）")
	}
}

// edgeReachable reports whether a request to url gets past Anthropic's edge: any answer except the
// edge's block page counts (an unauthenticated call gets 401); no answer at all does not.
func edgeReachable(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return !netBlockedText.Match(body)
}

func (nb *netBlock) summaryLocked() string {
	var b strings.Builder
	fmt.Fprintf(&b, "丢了 %d 轮，恢复后重跑：\n", len(nb.lost)+nb.more)
	for _, l := range nb.lost {
		b.WriteString("- " + l.task + "：" + clipRunes(oneLine(l.input), 80) + "\n")
	}
	if nb.more > 0 {
		fmt.Fprintf(&b, "- 另有 %d 轮超出记录上限，不会自动重跑\n", nb.more)
	}
	return strings.TrimSpace(b.String())
}

// netRecovered closes the to-do and runs the lost rounds again, each once.
func (s *Server) netRecovered(outcome string) {
	nb := &s.net
	nb.mu.Lock()
	lost := nb.lost
	nb.lost, nb.more, nb.account = nil, 0, false
	nb.mu.Unlock()
	for _, ref := range []string{netBlockedRef, accountBlockedRef} {
		if err := s.resolveSystem(ref, outcome); err != nil {
			s.logf("blocked: close to-do %s: %v", ref, err)
		}
	}
	for _, l := range lost {
		if m := handoffTodo.FindStringSubmatch(l.input); m != nil {
			_, _ = s.Inbox.Resend(m[1]) // the answer is on its way again: the card stops saying it failed
		}
		s.Engine.Submit(runner.Request{TaskID: l.task, Trigger: runner.ByRetry, Input: netRerunNote + l.input})
	}
	if len(lost) > 0 {
		s.logf("net blocked: re-running %d lost rounds", len(lost))
	}
}

func (s *Server) runRecord(task, id string) (rec store.RunRecord, ok bool) {
	runs, err := s.Store.Runs(task, 50)
	if err != nil {
		return rec, false
	}
	for _, r := range runs {
		if r.ID == id {
			return r, true
		}
	}
	return rec, false
}

func contains(list []netLost, x netLost) bool {
	for _, l := range list {
		if l == x {
			return true
		}
	}
	return false
}

func without(list []netLost, x netLost) []netLost {
	out := list[:0:0]
	for _, l := range list {
		if l != x {
			out = append(out, l)
		}
	}
	return out
}
