package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"
)

type fakeAdapter struct{}

func (fakeAdapter) Available() bool       { return true }
func (fakeAdapter) Models() []ModelOption { return []ModelOption{{"m1", "M1"}, {"m2", "M2"}} }
func (fakeAdapter) ModelSource() string   { return "固定定義" }
func (fakeAdapter) SelfTool() bool        { return true }
func (fakeAdapter) Run(context.Context, string, string, string, string) (TurnResult, error) {
	return TurnResult{}, nil
}

func newTestRoom(t *testing.T) (*Room, *Agent) {
	a := &Agent{ID: "x", Name: "X", Adapter: fakeAdapter{}}
	r := NewRoom([]*Agent{a}, t.TempDir(), t.TempDir(), 10, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return r, a
}

func TestSetModelByAgent(t *testing.T) {
	r, a := newTestRoom(t)
	r.jobs["tok"] = &jobTicket{agentID: "x", gen: r.gen, modelVer: a.modelVer}

	if _, err := r.setModelByAgent("other", "m1"); err == nil {
		t.Fatal("無効なトークンを受け付けた")
	}
	if _, err := r.setModelByAgent("tok", "zzz"); err == nil {
		t.Fatal("候補にないモデルを受け付けた")
	}
	if _, err := r.setModelByAgent("tok", "m1"); err != nil || a.modelSel != "m1" {
		t.Fatalf("変更できない: %v %q", err, a.modelSel)
	}
	if _, err := r.setModelByAgent("tok", "m2"); err != nil || a.modelSel != "m2" {
		t.Fatalf("同じジョブからの続けての変更ができない: %v", err)
	}
	// ジョブ実行中に人間が変更したら、そのジョブからの要求は拒否する
	if err := r.SetAgentModel("x", "m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.setModelByAgent("tok", "m2"); err == nil || a.modelSel != "m1" {
		t.Fatal("人間の変更を古いジョブが上書きした")
	}
	// 会話のリセット後は拒否する
	r.jobs["tok2"] = &jobTicket{agentID: "x", gen: r.gen, modelVer: a.modelVer}
	r.gen++
	if _, err := r.setModelByAgent("tok2", "m2"); err == nil {
		t.Fatal("世代の異なるジョブからの要求を受け付けた")
	}
}

func TestUsageTotal(t *testing.T) {
	var u UsageTotal
	u.add(Usage{Known: true, Input: 10, CachedInput: 4, Output: 2})
	u.add(Usage{})
	if u.Jobs != 2 || u.UnknownJobs != 1 || u.Input != 10 || u.Output != 2 {
		t.Fatalf("%+v", u)
	}
}

func TestParseUsageFromPartialOutput(t *testing.T) {
	// キャンセル直前に使用量イベントまで出力されていた場合も集計できる
	e := parseCodexEvents(`{"type":"thread.started","thread_id":"t1"}
{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":7}}`)
	if !e.usage.Known || e.usage.Input != 100 || e.usage.CachedInput != 40 || e.usage.Output != 7 {
		t.Fatalf("%+v", e.usage)
	}
	if e := parseCodexEvents(`{"type":"thread.started","thread_id":"t1"}`); e.usage.Known {
		t.Fatal("使用量がないのに取得済みになった")
	}
	j, ok := parseClaudeOutput(`{"type":"assistant","message":{"content":[]}}
{"type":"result","result":"x","modelUsage":{"m":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4}}}`)
	_, u := j.modelAndUsage()
	if !ok || u.Input != 8 || u.CachedInput != 3 || u.Output != 2 {
		t.Fatalf("%+v", u)
	}
	if j, _ := parseClaudeOutput(""); func() bool { _, u := j.modelAndUsage(); return u.Known }() {
		t.Fatal("空の出力で取得済みになった")
	}
	// 文脈の大きさは最後の assistant 行（サブエージェント分を除く）の入力で、ツール呼び出しの回数では膨らまない
	out := `{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":1,"cache_read_input_tokens":100000,"cache_creation_input_tokens":0}}}
{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":2,"cache_read_input_tokens":110000,"cache_creation_input_tokens":500}}}
{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"usage":{"input_tokens":3,"cache_read_input_tokens":900000,"cache_creation_input_tokens":0}}}
{"type":"result","result":"x","modelUsage":{"m":{"inputTokens":3,"outputTokens":2,"cacheReadInputTokens":210000,"cacheCreationInputTokens":500}}}`
	if c := claudeContextTokens(out); c != 110502 {
		t.Fatalf("文脈の大きさ: %d", c)
	}
	if c := claudeContextTokens(`{"type":"result","result":"x"}`); c != 0 {
		t.Fatalf("assistant 行がないのに %d", c)
	}
}

func TestSelfToolAfterStopAndRedaction(t *testing.T) {
	r, a := newTestRoom(t)
	r.jobs["secret-token"] = &jobTicket{agentID: "x", gen: r.gen, modelVer: a.modelVer}
	// 不正なモデル名に含まれたトークンは応答に出さない
	_, err := r.setModelByAgent("secret-token", "secret-token")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("トークンが伏せられていない: %v", err)
	}
	// 停止後は、ジョブの終了前でも要求を拒否する
	r.Stop()
	if _, err := r.setModelByAgent("secret-token", "m1"); err == nil || a.modelSel != "" {
		t.Fatal("停止後の要求を受け付けた")
	}
}

type failingAdapter struct{ fakeAdapter }

// CLI のエラー出力にプロンプト（トークンを含む）がそのまま出る場合を模す
func (failingAdapter) Run(_ context.Context, prompt, _, _, _ string) (TurnResult, error) {
	return TurnResult{}, &AgentError{ErrAgentFailed, "failed: " + prompt}
}

func TestTokenRedactedInErrorMessage(t *testing.T) {
	r, a := newTestRoom(t)
	a.Adapter = failingAdapter{}
	selfToolURL = "http://127.0.0.1:0/mcp"
	defer func() { selfToolURL = "" }()
	r.mu.Lock()
	r.postLocked("human", "hi", "chat")
	r.mu.Unlock()
	r.runTurn(context.Background(), a)
	r.mu.Lock()
	defer r.mu.Unlock()
	last := r.messages[len(r.messages)-1]
	if last.Kind != "system" || !strings.Contains(last.Text, `token="`+redactedToken+`"`) || regexp.MustCompile(`[0-9a-f]{32}`).MatchString(last.Text) {
		t.Fatalf("エラー文のトークンが伏せられていない: %s", last.Text)
	}
}

type replyAdapter struct{ fakeAdapter }

// 返答中に届いた発言があっても、返答には書き始めた時点の既読位置が付く
func (replyAdapter) Run(_ context.Context, prompt, _, _, _ string) (TurnResult, error) {
	return TurnResult{Text: "了解"}, nil
}

func TestReplyHasReadUpTo(t *testing.T) {
	r, a := newTestRoom(t)
	a.Adapter = replyAdapter{}
	r.mu.Lock()
	r.postLocked("human", "hi", "chat")
	first := r.messages[len(r.messages)-1].ID
	r.mu.Unlock()
	r.runTurn(context.Background(), a)
	r.mu.Lock()
	defer r.mu.Unlock()
	last := r.messages[len(r.messages)-1]
	if last.From != "x" || last.ReadUpTo != first {
		t.Fatalf("返答の既読位置 = %d, want %d (%+v)", last.ReadUpTo, first, last)
	}
	if got := formatDelta(last); got != fmt.Sprintf("[#%d x（#%d まで読了）]: 了解", last.ID, first) {
		t.Fatalf("formatDelta = %q", got)
	}
}
