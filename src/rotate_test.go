package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rotateAdapter は新しいセッションでの実行（議事録の作成・切り替え後のターン）と、既存セッションでの実行を記録する
type rotateAdapter struct {
	fakeAdapter
	fail    bool // 議事録の作成を失敗させる
	prompts chan string
}

func (s rotateAdapter) Run(_ context.Context, prompt, sessionID, _, _ string) (TurnResult, error) {
	s.prompts <- sessionID + "|" + prompt
	if isMinutesPrompt(prompt) {
		if s.fail {
			return TurnResult{}, errors.New("boom")
		}
		return TurnResult{Text: "## 決定事項\n- A にする"}, nil
	}
	return TurnResult{Text: "了解", SessionID: "sess-new", Usage: Usage{Known: true, Input: 100}}, nil
}

// isMinutesPrompt は議事録（要約）を作るプロンプトか（全文・差分のどちらでも）
func isMinutesPrompt(p string) bool {
	return strings.HasPrefix(p, summaryInstruction) || strings.HasPrefix(p, summaryDeltaInstruction)
}

func setupRotate(t *testing.T, fail bool) (*Room, *Agent, rotateAdapter) {
	r, a := newTestRoom(t)
	ad := rotateAdapter{fail: fail, prompts: make(chan string, 10)}
	a.Adapter = ad
	r.mu.Lock()
	r.postLocked("human", "A と B どちらにする？", "chat")
	r.postLocked("x", "A がよいです", "chat")
	a.sessionID, a.cursor, a.lastInput = "sess-old", len(r.messages), 600000
	r.mu.Unlock()
	return r, a, ad
}

// しきい値を超えたエージェントは、議事録を作ってから新しいセッションで議事録のパスと直近の発言を受け取る
func TestRotateSession(t *testing.T) {
	r, a, ad := setupRotate(t, false)
	r.mu.Lock()
	r.postLocked("human", "続きをお願いします", "chat")
	r.checkRotateLocked()
	if !a.rotatePending || r.minutes == nil {
		r.mu.Unlock()
		t.Fatal("切り替えが予約されていない")
	}
	r.mu.Unlock()

	if out := r.runTurn(context.Background(), a); out != outcomeSpoke {
		t.Fatalf("outcome = %v", out)
	}
	minutes := <-ad.prompts
	if !strings.HasPrefix(minutes, "|"+summaryInstruction) || !strings.Contains(minutes, "A と B どちらにする？") {
		t.Fatalf("議事録が新しいセッションで会話の記録から作られていない: %.80q", minutes)
	}
	turn := <-ad.prompts
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastMinutes == nil {
		t.Fatal("議事録のパスが記録されていない")
	}
	if b, err := os.ReadFile(r.lastMinutes.path); err != nil || !strings.Contains(string(b), "A にする") {
		t.Fatalf("議事録が保存されていない: %v", err)
	}
	if !strings.HasPrefix(turn, "|") {
		t.Fatalf("古いセッションを引き継いでいる: %.40q", turn)
	}
	if !strings.Contains(turn, r.lastMinutes.path) || !strings.Contains(turn, "A がよいです") || !strings.Contains(turn, "続きをお願いします") {
		t.Fatalf("議事録のパスか直近の発言が渡っていない: %s", turn)
	}
	if a.sessionID != "sess-new" || a.rotatePending || a.lastInput != 100 || a.sessionStart == 0 {
		t.Fatalf("状態: session=%q pending=%v last=%d", a.sessionID, a.rotatePending, a.lastInput)
	}
}

// 議事録の作成に失敗したら、切り替えずに元のセッションで続けて通知する
func TestRotateSessionMinutesFailed(t *testing.T) {
	r, a, ad := setupRotate(t, true)
	r.mu.Lock()
	r.postLocked("human", "続きをお願いします", "chat")
	r.checkRotateLocked()
	r.mu.Unlock()

	r.runTurn(context.Background(), a)
	<-ad.prompts
	if turn := <-ad.prompts; !strings.HasPrefix(turn, "sess-old|") {
		t.Fatalf("元のセッションで続けていない: %.40q", turn)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	found := false
	for _, m := range r.messages {
		found = found || strings.Contains(m.Text, "議事録の作成に失敗しました")
	}
	if !found || a.rotatePending || r.lastMinutes != nil {
		t.Fatalf("失敗の通知または予約の取り消しがない: found=%v pending=%v", found, a.rotatePending)
	}
}

// しきい値以下・0（無効）のときは切り替えない
func TestRotateThreshold(t *testing.T) {
	r, a, _ := setupRotate(t, false)
	r.mu.Lock()
	defer r.mu.Unlock()
	a.lastInput = defaultRotateTokens
	r.checkRotateLocked()
	if a.rotatePending {
		t.Fatal("しきい値ちょうどで切り替えた")
	}
	a.lastInput = defaultRotateTokens + 1
	r.rotateTokens = 0
	r.checkRotateLocked()
	if a.rotatePending {
		t.Fatal("しきい値 0（無効）で切り替えた")
	}
}

// 使用量を取得できないエージェントは、セッションに渡した発言の件数で判定する
func TestRotateUnknownUsage(t *testing.T) {
	r, a, _ := setupRotate(t, false)
	r.mu.Lock()
	a.lastInput, a.sessionStart = 0, 1
	r.checkRotateLocked()
	if a.rotatePending {
		r.mu.Unlock()
		t.Fatal("発言が少ないのに切り替えた")
	}
	r.nextID = a.sessionStart + rotateUnknownMsgs + 1
	r.checkRotateLocked()
	if !a.rotatePending {
		r.mu.Unlock()
		t.Fatal("発言数が上限を超えても切り替えない")
	}
	job := r.minutes
	r.mu.Unlock()
	<-job.done // 議事録の作成（ロックを取る）の終了を待ってから終える
}

// 開始位置のない古い形式の session.json を復元しても、会話全体の発言数で切り替えない
func TestRestoreOldSessionNoRotate(t *testing.T) {
	r, _ := newTestRoom(t)
	r.mu.Lock()
	for i := range rotateUnknownMsgs + 10 {
		r.postLocked("human", fmt.Sprintf("発言%d", i), "chat")
	}
	old := fmt.Sprintf(`{"log_file":%q,"workdir":%q,"agents":{"x":{"session_id":"sess-old","cursor":%d}}}`,
		filepath.Base(r.logFile), r.workdir, len(r.messages))
	r.mu.Unlock()
	if err := os.WriteFile(r.sessionPath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	r2, a2 := newTestRoom(t)
	r2.logDir, r2.workdir = r.logDir, r.workdir
	r2.mu.Lock()
	defer r2.mu.Unlock()
	r2.restoreSessionLocked()
	if a2.sessionID != "sess-old" || a2.sessionStart != r2.nextID {
		t.Fatalf("復元: session=%q start=%d next=%d", a2.sessionID, a2.sessionStart, r2.nextID)
	}
	r2.checkRotateLocked()
	if a2.rotatePending {
		t.Fatal("古い形式の復元直後に切り替えた")
	}
}

// 新着がなければ、切り替えを確定せずに次の新着があるターンまで持ち越す
func TestRotateDeferredWithoutDelta(t *testing.T) {
	r, a, ad := setupRotate(t, false)
	r.mu.Lock()
	r.postLocked("human", "続きをお願いします", "chat")
	r.checkRotateLocked()
	job := r.minutes
	r.mu.Unlock()
	<-ad.prompts // 議事録の作成
	<-job.done
	r.mu.Lock()
	a.cursor = len(r.messages) // 新着なし
	r.mu.Unlock()

	if out := r.runTurn(context.Background(), a); out != outcomeSkipped {
		t.Fatalf("outcome = %v", out)
	}
	r.mu.Lock()
	if a.state == "thinking" || a.sessionID != "sess-old" || !a.rotatePending {
		r.mu.Unlock()
		t.Fatalf("state=%q session=%q pending=%v", a.state, a.sessionID, a.rotatePending)
	}
	r.postLocked("human", "次の依頼", "chat")
	r.mu.Unlock()

	r.runTurn(context.Background(), a)
	r.mu.Lock()
	defer r.mu.Unlock()
	if turn := <-ad.prompts; !strings.HasPrefix(turn, "|") || !strings.Contains(turn, r.lastMinutes.path) || !strings.Contains(turn, "次の依頼") {
		t.Fatalf("持ち越した切り替えで議事録が渡っていない: %s", turn)
	}
}

// 議事録を作った後の発言は、直近の件数を超えてもすべて渡す
func TestRotatePassesMessagesSinceMinutes(t *testing.T) {
	r, a, ad := setupRotate(t, false)
	r.mu.Lock()
	r.postLocked("human", "続きをお願いします", "chat")
	r.checkRotateLocked()
	job := r.minutes
	r.mu.Unlock()
	<-ad.prompts
	<-job.done
	r.mu.Lock()
	for i := range rotateRecentMsgs + 5 {
		r.postLocked("human", fmt.Sprintf("追加の発言%d", i), "chat")
	}
	r.mu.Unlock()

	r.runTurn(context.Background(), a)
	turn := <-ad.prompts
	for _, want := range []string{"追加の発言0", fmt.Sprintf("追加の発言%d", rotateRecentMsgs+4)} {
		if !strings.Contains(turn, want) {
			t.Fatalf("%q が渡っていない", want)
		}
	}
}

// 切り替え後の最初のターンが失敗したら、次のターンで議事録を渡し直す
type failOnceAdapter struct {
	rotateAdapter
	failed *bool
}

func (s failOnceAdapter) Run(ctx context.Context, prompt, sessionID, model, cwd string) (TurnResult, error) {
	if !isMinutesPrompt(prompt) && !*s.failed {
		*s.failed = true
		s.prompts <- sessionID + "|" + prompt
		return TurnResult{}, errors.New("boom")
	}
	return s.rotateAdapter.Run(ctx, prompt, sessionID, model, cwd)
}

func TestRotateRetryAfterFailure(t *testing.T) {
	r, a, ad := setupRotate(t, false)
	a.Adapter = failOnceAdapter{rotateAdapter: ad, failed: new(bool)}
	r.mu.Lock()
	r.postLocked("human", "続きをお願いします", "chat")
	r.checkRotateLocked()
	r.mu.Unlock()

	if out := r.runTurn(context.Background(), a); out != outcomeFailed {
		t.Fatalf("outcome = %v", out)
	}
	<-ad.prompts // 議事録の作成
	<-ad.prompts // 失敗したターン
	r.mu.Lock()
	if !a.rotatePending {
		r.mu.Unlock()
		t.Fatal("失敗後に切り替えの予約が戻っていない")
	}
	r.mu.Unlock()
	r.runTurn(context.Background(), a)
	r.mu.Lock()
	defer r.mu.Unlock()
	if turn := <-ad.prompts; !strings.HasPrefix(turn, "|") || !strings.Contains(turn, r.lastMinutes.path) {
		t.Fatalf("議事録が渡し直されていない: %.200s", turn)
	}
}

// rotateOnce は切り替えを予約して議事録の作成を終えるまで待ち、議事録を作ったプロンプトを返す
func rotateOnce(t *testing.T, r *Room, a *Agent, ad rotateAdapter) string {
	t.Helper()
	r.mu.Lock()
	a.sessionID, a.lastInput = "sess-old", 600000
	r.checkRotateLocked()
	job := r.minutes
	r.mu.Unlock()
	if job == nil {
		t.Fatal("切り替えが予約されていない")
	}
	prompt := <-ad.prompts
	<-job.done
	r.mu.Lock()
	defer r.mu.Unlock()
	if job.err != nil {
		t.Fatalf("議事録の作成に失敗: %v", job.err)
	}
	a.rotatePending = false
	return prompt
}

// 2回目以降の議事録は、前回の議事録とその後の発言から作る（前回より前の発言は渡さない）（案10）
func TestMinutesDelta(t *testing.T) {
	r, a, ad := setupRotate(t, false)
	first := rotateOnce(t, r, a, ad)
	if !strings.HasPrefix(first, "|"+summaryInstruction) {
		t.Fatalf("1回目が全文から作られていない: %.80q", first)
	}
	r.mu.Lock()
	r.postLocked("human", "次は C を検討して", "chat")
	r.mu.Unlock()

	second := rotateOnce(t, r, a, ad)
	if !strings.HasPrefix(second, "|"+summaryDeltaInstruction) {
		t.Fatalf("2回目が差分から作られていない: %.80q", second)
	}
	prev, delta, ok := strings.Cut(second, summaryDeltaSeparator)
	if !ok {
		t.Fatal("前回の議事録と新しい記録の区切りがない")
	}
	if !strings.Contains(prev, "A にする") {
		t.Fatalf("前回の議事録（決定事項）が渡っていない: %s", prev)
	}
	if !strings.Contains(delta, "次は C を検討して") || strings.Contains(delta, "A と B どちらにする？") {
		t.Fatalf("新しい記録が前回の議事録の後の発言だけになっていない: %s", delta)
	}
}

// 前回の議事録が読めなければ、全文から作り直す（案10）
func TestMinutesDeltaFallbackToFull(t *testing.T) {
	r, a, ad := setupRotate(t, false)
	rotateOnce(t, r, a, ad)
	r.mu.Lock()
	if err := os.Remove(r.lastMinutes.path); err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	r.postLocked("human", "次は C を検討して", "chat")
	r.mu.Unlock()

	p := rotateOnce(t, r, a, ad)
	if !strings.HasPrefix(p, "|"+summaryInstruction) || !strings.Contains(p, "A と B どちらにする？") || !strings.Contains(p, "次は C を検討して") {
		t.Fatalf("全文から作り直していない: %.200q", p)
	}
}

// 前回の議事録の後に発言がなければ、全文から作る（案10）
func TestMinutesDeltaNoNewMessages(t *testing.T) {
	r, a, ad := setupRotate(t, false)
	rotateOnce(t, r, a, ad)
	r.mu.Lock()
	r.messages = r.messages[:r.lastMinutes.upto] // 議事録の作成後の system の発言を除いて「新着なし」にする
	p, mode := r.minutesPromptLocked()
	r.mu.Unlock()
	if mode != "full" || !strings.HasPrefix(p, summaryInstruction) {
		t.Fatalf("mode=%s prompt=%.80q", mode, p)
	}
}
