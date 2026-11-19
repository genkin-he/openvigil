//go:build realclaude

package replay

import (
	"context"
	"testing"
	"time"
)

// Run with: go test ./internal/replay/ -tags realclaude -run TestRealToolless
func TestRealToolless(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := Runner{Bin: "claude", Model: "sonnet"}
	d, cost, err := r.Decide(ctx, "群里的机器人通知不用处理；同事问进度时，有证据就直接回。",
		Case{RunID: "real", Input: "SLACK C1:1.1 user=bot | 【部署通知】demo-service 发布完成"})
	if err != nil || !d.Valid {
		t.Fatalf("Decide = %+v %v", d, err)
	}
	t.Logf("decision %+v, cost $%.4f", d, cost)
}
