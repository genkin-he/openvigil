package server

import (
	"net/http"
	"strings"
)

// The owner's principles (store.ReadOwner): read by every task, kept up by the self-evolution task.

type ownerDoc struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (s *Server) ownerGet(_ http.ResponseWriter, _ *http.Request) (any, error) {
	content, err := s.Store.ReadOwner()
	if err != nil {
		return nil, err
	}
	return ownerDoc{Path: s.Store.OwnerPath(), Content: content}, nil
}

func (s *Server) ownerAppend(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b struct {
		Text string `json:"text"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if strings.TrimSpace(b.Text) == "" {
		return nil, badRequest("原则不能为空")
	}
	if err := s.Store.AppendOwner(b.Text, s.Now()); err != nil {
		return nil, err
	}
	return map[string]string{"hint": "已写进我的判断原则：" + s.Store.OwnerPath()}, nil
}

func (s *Server) ownerPut(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b struct {
		Content string `json:"content"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if err := s.Store.WriteOwner(b.Content); err != nil {
		return nil, err
	}
	return map[string]string{"hint": "已替换我的判断原则，旧版存在 history/ 下"}, nil
}
