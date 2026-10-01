package main

import (
	"context"
	"testing"
)

type quotaErrAdapter struct{ fakeAdapter }

func (quotaErrAdapter) Run(context.Context, string, string, string, string) (TurnResult, error) {
	return TurnResult{}, cliFailure("codex: You've hit your usage limit. Upgrade to Pro or try again at 7:51 PM.")
}

// 返答本文（stdout）に「usage limit」が含まれていても、利用枠のエラーとは扱わない
func TestQuotaNotFromStdout(t *testing.T) {
	err := procFailure("claude", procResult{stdout: `{"type":"assistant","message":{"content":[{"type":"text","text":"usage limit の判定"}]}}`, exitCode: 1})
	if ae := err.(*AgentError); ae.Code != ErrAgentFailed {
		t.Fatalf("stdout で利用枠のエラーと判定された: %s", ae.Code)
	}
	err = procFailure("claude", procResult{stderr: "Error: Claude AI usage limit reached", exitCode: 1})
	if ae := err.(*AgentError); ae.Code != ErrAgentQuota {
		t.Fatalf("stderr の利用枠のエラーを判定できない: %s", ae.Code)
	}
}

// 利用枠の上限エラーで自動的に一時停止になり、宛先・進行役から外れる。人間の操作で戻る
func TestAutoPauseOnQuotaError(t *testing.T) {
	r, a := newTestRoom(t)
	a.Adapter = quotaErrAdapter{}
	r.leader = "x"
	r.mu.Lock()
	r.postLocked("human", "hi", "chat")
	r.mu.Unlock()
	if out := r.runTurn(context.Background(), a); out != outcomeFailed {
		t.Fatalf("outcome = %v", out)
	}
	r.mu.Lock()
	if !a.paused || r.leader != "" || len(r.activeIDs("")) != 0 {
		t.Fatalf("一時停止になっていない: paused=%v leader=%q active=%v", a.paused, r.leader, r.activeIDs(""))
	}
	r.postLocked("human", "もう一度", "chat")
	r.mu.Unlock()
	if out := r.runTurn(context.Background(), a); out != outcomeSkipped {
		t.Fatalf("一時停止中にターンが実行された: %v", out)
	}
	if err := r.SetLeader("x"); err == nil {
		t.Fatal("一時停止中のエージェントを進行役にできた")
	}

	// 再起動後も一時停止のまま
	r2, a2 := newTestRoom(t)
	r2.logDir = r.logDir
	r2.mu.Lock()
	r2.restoreSessionLocked()
	r2.mu.Unlock()
	if !a2.paused {
		t.Fatal("再起動後に一時停止が戻っていない")
	}

	if err := r.SetPaused("x", false); err != nil || a.paused {
		t.Fatalf("再開できない: %v", err)
	}
}

func TestIsQuotaError(t *testing.T) {
	for msg, want := range map[string]bool{
		"You've hit your usage limit.":          true,
		"Claude AI usage limit reached|1790000": true,
		"RESOURCE_EXHAUSTED: quota":             true,
		"agy: Rate limit reached for requests":  true,
		"exit status 1: file not found":         false,
		"中断されました":                               false,
	} {
		if got := isQuotaError(msg); got != want {
			t.Errorf("%q: got %v", msg, got)
		}
	}
}
