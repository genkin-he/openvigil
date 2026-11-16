package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"vigil/internal/config"
)

type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient() (*client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return &client{base: cfg.URL(), token: cfg.Token, http: &http.Client{Timeout: 2 * time.Minute}}, nil
}

// call sends a JSON request and decodes the envelope's data into out (when non-nil).
func (c *client) call(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("连不上守护进程（%s）：%w。先执行 vigil install，或用 vigil daemon 前台运行", c.base, err)
	}
	defer resp.Body.Close()
	var env struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode response (%s): %w", resp.Status, err)
	}
	if !env.OK {
		if env.Error == "" {
			env.Error = resp.Status
		}
		return errors.New(env.Error)
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

// callRaw posts a raw body (vigil done passes the agent's JSON through untouched).
func (c *client) callRaw(path string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("连不上守护进程：%w", err)
	}
	defer resp.Body.Close()
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode response (%s): %w", resp.Status, err)
	}
	if !env.OK {
		return errors.New(env.Error)
	}
	return nil
}
