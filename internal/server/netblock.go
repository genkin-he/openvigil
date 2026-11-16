package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"vigil/internal/runner"
	"vigil/internal/store"
)

// A blocked network: Anthropic's edge refuses requests from the current proxy / VPN exit (403
// network_edge_blocked, or a Cloudflare challenge page). Every run fails the same way until the exit
// changes, so the failures become one system to-do instead of a pile of red runs, and the rounds that
// were lost are run again once a run gets through (or the owner says it is fixed).
const (
	netBlockedRef  = "net:blocked"
	netBlockedKeep = 20 // lost rounds remembered for a re-run; older ones are only counted
	netRerunNote   = "（这一轮之前因为网络被拦没跑完，现在重跑。先确认上次有没有已经做过的动作，别重复发消息、别重复建待办。）\n"
)

var netBlockedText = regexp.MustCompile(`network_edge_blocked|Request blocked before it reached Anthropic|Attention Required! \| Cloudflare`)

// netLost is a round that failed on the blocked network, kept to run again.
type netLost struct {
	task, input string
}

type netBlock struct {
	mu   sync.Mutex
	lost []netLost
	more int // lost rounds beyond netBlockedKeep
}

// NetWatchLoop follows finished runs for a blocked network until ctx ends.
func (s *Server) NetWatchLoop(ctx context.Context) {
	events, stop := s.Engine.Hub().Subscribe()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
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

func (s *Server) noteRunForNet(ev runner.Event) {
	rec, ok := s.runRecord(ev.TaskID, ev.RunID)
	if !ok || rec.Job != "" { // background jobs are not re-run: their parent decides
		return
	}
	nb := &s.net
	switch {
	case rec.Status == "ok" || rec.Status == "needs_you":
		nb.mu.Lock()
		blocked := len(nb.lost) > 0 || nb.more > 0
		nb.lost = without(nb.lost, netLost{rec.TaskID, rec.Input}) // got through on its own retry
		nb.mu.Unlock()
		if blocked {
			s.netRecovered("网络恢复了（" + rec.TaskID + " 这一轮跑通了）")
		}
	case netBlockedText.MatchString(rec.Error):
		nb.mu.Lock()
		lost := netLost{rec.TaskID, rec.Input}
		if !contains(nb.lost, lost) {
			if len(nb.lost) < netBlockedKeep {
				nb.lost = append(nb.lost, lost)
			} else {
				nb.more++
			}
		}
		summary := nb.summaryLocked()
		nb.mu.Unlock()
		if err := s.raiseSystem(netBlockedRef, "网络被拦：Claude 的请求被 Anthropic 挡下",
			"代理 / VPN 当前的出口地区或节点被拒（403 network_edge_blocked）。换一个支持的地区或节点，"+
				"然后点「复查」立刻重跑丢掉的几轮；不点的话，下一轮跑通时也会自动重跑。", summary); err != nil {
			s.logf("net blocked: raise to-do: %v", err)
		}
	}
}

func (nb *netBlock) summaryLocked() string {
	var b strings.Builder
	fmt.Fprintf(&b, "丢了 %d 轮，网络恢复后重跑：\n", len(nb.lost)+nb.more)
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
	nb.lost, nb.more = nil, 0
	nb.mu.Unlock()
	if err := s.resolveSystem(netBlockedRef, outcome); err != nil {
		s.logf("net blocked: close to-do: %v", err)
	}
	for _, l := range lost {
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
