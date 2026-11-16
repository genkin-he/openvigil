package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/patch"
)

// Changes to Vigil's own code: an agent proposes a diff (vigil patch propose), the owner reads it on a
// card and approves it in the console, and only then does plain code here apply, test, merge and
// install it. An agent never lands a change to the system that oversees it; and since agent sends show
// up as the owner in Slack, a patch card cannot be approved from Slack either.
const (
	patchRefPrefix   = "patch:"
	patchKind        = "改代码"
	patchLandTimeout = 15 * time.Minute
	patchInstallWait = 30 * time.Minute // an install that has not reported by then is taken as failed
	patchPollEvery   = 15 * time.Second
)

// Patcher lands approved patches. Nil turns proposals off (no source_dir in config.yaml).
type Patcher struct {
	Store patch.Store
	// Land applies a diff in a scratch worktree, tests it, and fast-forwards main; it returns the commit.
	Land func(ctx context.Context, diffPath, message string) (string, error)
	// Install starts installing the new build without waiting: it restarts this daemon, and writes the
	// exit code to the store's InstallExit when done.
	Install func(id string) error
}

func isPatch(it inbox.Item) bool { return strings.HasPrefix(it.Ref, patchRefPrefix) }

type patchRequest struct {
	Task  string `json:"task"`
	Title string `json:"title"`
	Why   string `json:"why"`
	Diff  string `json:"diff"`
}

// patchAdd saves a proposal and raises its card.
func (s *Server) patchAdd(_ http.ResponseWriter, r *http.Request) (any, error) {
	if s.Patcher == nil {
		return nil, badRequest("没开改代码提议：在 config.yaml 里写 source_dir（Vigil 源码仓库的路径）")
	}
	var b patchRequest
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if _, err := s.Store.Get(strings.TrimSpace(b.Task)); err != nil {
		return nil, badRequest("没有任务 %s", b.Task)
	}
	p, err := s.Patcher.Store.Add(patch.Proposal{Task: b.Task, Title: b.Title, Why: strings.TrimSpace(b.Why)}, b.Diff, s.Now())
	if err != nil {
		return nil, badRequest("%v", err)
	}
	it, _, err := s.Inbox.Add(inbox.Item{TaskID: p.Task, Kind: patchKind, Title: "改 Vigil 代码：" + p.Title,
		Analysis: p.Why, Reply: b.Diff, Ref: patchRefPrefix + p.ID})
	if err != nil {
		return nil, err
	}
	s.todoChanged(it, "todo.added")
	return map[string]any{"patch": p, "todo": it.ID}, nil
}

// settlePatch is the owner's answer to a patch card: apply it, or not.
func (s *Server) settlePatch(it inbox.Item, mode string) (inbox.Item, error) {
	if it.Status != inbox.StatusOpen {
		return inbox.Item{}, todoErr(inbox.ErrClosed)
	}
	if s.Patcher == nil {
		return inbox.Item{}, badRequest("没开改代码提议：config.yaml 里没有 source_dir")
	}
	id := strings.TrimPrefix(it.Ref, patchRefPrefix)
	p, err := s.Patcher.Store.Get(id)
	if err != nil {
		return inbox.Item{}, badRequest("%v", err)
	}
	switch mode {
	case TodoDismiss:
		closed, err := s.Inbox.CloseIfOpen(it.ID, "没采纳，代码没动")
		if err != nil {
			return inbox.Item{}, todoErr(err)
		}
		if closed, err = s.Inbox.SetDecision(it.ID, inbox.DecisionDismissed); err != nil {
			return inbox.Item{}, todoErr(err)
		}
		s.savePatch(p, patch.StatusDeclined, "")
		s.todoChanged(closed, "todo.closed")
		return closed, nil
	case TodoSend:
	default:
		return inbox.Item{}, badRequest("这张卡只能「应用」或「不要」")
	}
	answered, err := s.Inbox.Answer(it.ID, "应用这个改动")
	if err != nil {
		return inbox.Item{}, todoErr(err) // settled elsewhere first
	}
	p = s.savePatch(p, patch.StatusApplying, "")
	s.todoChanged(answered, "todo.answered")
	s.background(func() { s.landPatch(answered, p) })
	return answered, nil
}

func (s *Server) landPatch(it inbox.Item, p patch.Proposal) {
	ctx, cancel := context.WithTimeout(context.Background(), patchLandTimeout)
	defer cancel()
	msg := "feat: " + p.Title + "\n\n" + p.Why + "\n\n由 " + p.Task + " 提议（" + p.ID + "），在 Vigil 控制台确认后自动应用。"
	sha, err := s.Patcher.Land(ctx, s.Patcher.Store.DiffPath(p.ID), msg)
	if err != nil {
		s.failPatch(it, p, err.Error())
		return
	}
	p.Commit = sha
	p = s.savePatch(p, patch.StatusInstalling, "")
	if err := s.Patcher.Install(p.ID); err != nil {
		s.failPatch(it, p, "已合进 main（"+short(sha)+"），但安装没启动："+err.Error())
		return
	}
	if noted, err := s.Inbox.Note(it.ID, "已合进 main（"+short(sha)+"），正在安装新版本；装好后这条自动结案。", false); err == nil {
		s.todoChanged(noted, "todo.updated")
	}
}

// failPatch puts the card back in front of the owner with what went wrong; the code is as it was
// unless the message says it was merged.
func (s *Server) failPatch(it inbox.Item, p patch.Proposal, why string) {
	s.savePatch(p, patch.StatusFailed, why)
	s.logf("patch %s: %s", p.ID, clipRunes(why, 300))
	noted, err := s.Inbox.Note(it.ID, "没应用成：\n"+why, true)
	if err != nil {
		s.logf("patch %s: note to-do: %v", p.ID, err)
		return
	}
	s.todoChanged(noted, "todo.noted")
}

func (s *Server) savePatch(p patch.Proposal, status, result string) patch.Proposal {
	p.Status, p.Result, p.Updated = status, result, s.Now()
	if err := s.Patcher.Store.Save(p); err != nil {
		s.logf("patch %s: save: %v", p.ID, err)
	}
	return p
}

// PatchLoop closes the cards of patches whose install has finished; the install restarts the daemon,
// so the outcome is picked up by whichever process is running when it lands.
func (s *Server) PatchLoop(ctx context.Context) {
	if s.Patcher == nil {
		return
	}
	tick := time.NewTicker(patchPollEvery)
	defer tick.Stop()
	for {
		s.finishInstalls()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Server) finishInstalls() {
	all, err := s.Patcher.Store.List()
	if err != nil {
		s.logf("patches: %v", err)
		return
	}
	for _, p := range all {
		if p.Status != patch.StatusInstalling {
			continue
		}
		it, found, err := s.Inbox.LatestByRef(patchRefPrefix + p.ID)
		if err != nil || !found {
			continue
		}
		code, done := installExit(s.Patcher.Store.InstallExit(p.ID))
		switch {
		case done && code == 0:
			s.savePatch(p, patch.StatusInstalled, "")
			if closed, err := s.Inbox.Close(it.ID, "已上线（提交 "+short(p.Commit)+"）"); err == nil {
				s.todoChanged(closed, "todo.closed")
			}
		case done:
			s.failPatch(it, p, "已合进 main（"+short(p.Commit)+"），但 make install 失败（退出码 "+strconv.Itoa(code)+"），线上还是旧版本：\n"+logTail(s.Patcher.Store.InstallLog(p.ID)))
		case s.Now().Sub(p.Updated) > patchInstallWait:
			s.failPatch(it, p, "已合进 main（"+short(p.Commit)+"），但安装 30 分钟都没有结果，看 "+s.Patcher.Store.InstallLog(p.ID))
		}
	}
}

func installExit(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1, true
	}
	return code, true
}

func logTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "（没有安装日志）"
		}
		return err.Error()
	}
	s := strings.TrimSpace(string(data))
	if r := []rune(s); len(r) > 1200 {
		s = "…" + string(r[len(r)-1200:])
	}
	return s
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
