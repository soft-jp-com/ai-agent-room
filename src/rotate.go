package main

// トークン消費対策: CLI セッションの切り替え。
// 人間が送信したとき、直近1回の実行の入力トークンがしきい値を超えたエージェントの CLI セッションを切り替える。
// 会話の議事録を1つ作って logs/minutes/ に保存し、切り替えたエージェント全員で使い回す。
// 切り替えたエージェントには、新しいセッションの最初のターンで議事録の絶対パスと直近の発言数件を渡す。
// チャットの履歴は消さない。議事録の作成に失敗したら、セッションを切り替えずに会話を続けて通知する。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultRotateTokens = 500000    // しきい値の既定（直近1回の実行の入力トークン。キャッシュ分を含む）
	maxRotateTokens     = 100000000 // しきい値の上限
	rotateRecentMsgs    = 10        // 切り替え後の最初のターンに渡す直近の発言数
	rotateUnknownMsgs   = 150       // 使用量を取得できないエージェントは、セッションに渡した発言がこの件数を超えたら切り替える
)

// minutesJob は議事録の作成1回分。done が閉じたら path（成功時）または err が確定する
type minutesJob struct {
	done chan struct{}
	upto int // 議事録に含めた発言数（r.messages の位置）
	path string
	err  error
}

// SetRotateTokens はセッションを切り替えるしきい値を変更する（0 なら切り替えない）
func (r *Room) SetRotateTokens(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rotateTokens = max(0, min(maxRotateTokens, n))
	r.pushStatusLocked()
}

// checkRotateLocked は人間の送信時に呼ばれ、しきい値を超えたエージェントのセッション切り替えを予約する。
// 議事録の作成は裏で行い、予約したエージェントは次のターンの開始時に完成を待つ。
func (r *Room) checkRotateLocked() {
	if r.rotateTokens <= 0 || r.minutes != nil {
		return
	}
	var targets []*Agent
	for _, a := range r.agents {
		if a.sessionID != "" && !a.rotatePending && r.needsRotateLocked(a) {
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		return
	}
	s := r.summarizerLocked()
	if s == nil {
		return
	}
	prompt, mode := r.minutesPromptLocked()
	if prompt == "" {
		return
	}
	names := make([]string, len(targets))
	ids := make([]string, len(targets))
	for i, a := range targets {
		a.rotatePending = true
		names[i] = fmt.Sprintf("%s（直近 %d トークン）", a.Name, a.lastInput)
		if a.lastInput == 0 {
			names[i] = fmt.Sprintf("%s（使用量不明、セッション内の発言 %d件）", a.Name, r.nextID-a.sessionStart)
		}
		ids[i] = a.ID
	}
	job := &minutesJob{done: make(chan struct{}), upto: len(r.messages)}
	r.minutes = job
	r.log.Info("session.rotate.start", "agents", strings.Join(ids, ","), "summarizer", s.ID, "threshold", r.rotateTokens, "mode", mode)
	r.postLocked("system", fmt.Sprintf("直近の入力がしきい値（%d トークン）を超えたため、%s の CLI セッションを切り替えます。%s が議事録を作成しています。",
		r.rotateTokens, strings.Join(names, "、"), s.Name), "system")
	ctx := withPermission(context.Background(), s.permission)
	ctx = withTurnTimeout(ctx, r.turnTimeoutForLocked(s))
	go r.writeMinutes(ctx, job, s, prompt, r.gen, r.workdir, s.modelSel, filepath.Base(r.logFile))
}

// needsRotateLocked はセッションを切り替えるべきかを判定する。直近1回の入力トークンがしきい値を超えたら切り替える。
// 今のセッションで使用量を一度も取得できていない（lastInput が 0）場合は、セッションに渡した発言の件数で判定する
func (r *Room) needsRotateLocked(a *Agent) bool {
	if a.lastInput > 0 {
		return a.lastInput > r.rotateTokens
	}
	return r.nextID-a.sessionStart > rotateUnknownMsgs
}

// writeMinutes は議事録を作成して logs/minutes/ に保存する。失敗したら予約を取り消して通知する
func (r *Room) writeMinutes(ctx context.Context, job *minutesJob, s *Agent, prompt string, gen int, workdir, modelSel, logName string) {
	start := time.Now()
	res, err := s.Adapter.Run(ctx, prompt, "", modelSel, workdir) // 議事録は新しいセッションで作る
	text := strings.TrimSpace(cleanReply(s, res.Text))
	if err == nil && text == "" {
		err = errors.New("議事録が空でした")
	}
	var path string
	if err == nil {
		dir := filepath.Join(r.logDir, "minutes")
		path = filepath.Join(dir, "minutes-"+time.Now().Format("20060102-150405")+".md")
		body := fmt.Sprintf("# 議事録\n\n- 会話ログ: %s\n- 作成: %s（%s）\n\n%s\n", logName, s.Name, time.Now().Format("2006-01-02 15:04:05"), text)
		if err = os.MkdirAll(dir, 0o755); err == nil {
			err = os.WriteFile(path, []byte(body), 0o644)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	s.usage.add(res.Usage)
	r.saveUsageLocked()
	log := r.log.With("summarizer", s.ID, "latency_ms", time.Since(start).Milliseconds())
	job.path, job.err = path, err
	close(job.done)
	if r.minutes == job {
		r.minutes = nil
	}
	if gen != r.gen { // 作成中に会話が変わった（resetLocked で予約は取り消し済み）
		log.Info("session.rotate.minutes", "discarded", "reset")
		return
	}
	if err != nil {
		log.Error("session.rotate.minutes", "error", err.Error())
		for _, a := range r.agents {
			a.rotatePending = false
		}
		r.postLocked("system", fmt.Sprintf("議事録の作成に失敗しました（%s）。セッションを切り替えずに会話を続けます。", err.Error()), "system")
		r.pushStatusLocked()
		return
	}
	r.lastMinutes = job
	log.Info("session.rotate.minutes", "path", path, "chars", len([]rune(text)))
	r.postLocked("system", fmt.Sprintf("議事録を作成しました: %s", path), "system")
}

// waitRotateLocked は runTurn の開始時に呼ばれ、セッションの切り替えが予約されていれば議事録を返す。
// 議事録の作成中なら、ロックを外して完成を待つ。予約がないか議事録の作成に失敗したら nil を返す。
// 待っている間に停止・リセットされたら ok=false を返す。切り替えの確定は、新着があるとわかってから commitRotateLocked で行う。
func (r *Room) waitRotateLocked(ctx context.Context, a *Agent) (job *minutesJob, ok bool) {
	if !a.rotatePending {
		return nil, true
	}
	if job = r.minutes; job != nil {
		gen := r.gen
		prev := a.state
		a.state, a.since, a.activity = "thinking", time.Now(), "議事録の作成を待っています"
		r.pushStatusLocked()
		r.mu.Unlock()
		select {
		case <-job.done:
		case <-ctx.Done():
		}
		r.mu.Lock()
		a.state, a.since, a.activity = prev, time.Time{}, "" // 新着があれば runTurn が改めて考え中にする
		r.pushStatusLocked()
		if ctx.Err() != nil || gen != r.gen {
			return nil, false
		}
		if job.err != nil || !a.rotatePending {
			return nil, true
		}
		return job, true
	}
	if r.lastMinutes == nil { // 通常は起こらない（予約は議事録の作成と同時に行う）
		a.rotatePending = false
		return nil, true
	}
	return r.lastMinutes, true
}

// commitRotateLocked はセッションの切り替えを確定し、新しいセッションに渡す発言を返す。
// 議事録に含めた後の発言をすべて渡す（直近 rotateRecentMsgs 件より少なければ直近 rotateRecentMsgs 件）。自分の発言も含め、パスは除く。
func (r *Room) commitRotateLocked(a *Agent, job *minutesJob, upto int) []Message {
	r.log.Info("session.rotate", "agent", a.ID, "old_session_id", a.sessionID, "last_input_tokens", a.lastInput, "minutes", job.path)
	a.sessionID, a.lastInput, a.rotatePending = "", 0, false
	r.saveSessionLocked()
	delta := recentMessages(r.messages[:upto], rotateRecentMsgs)
	if job.upto < upto {
		if since := recentMessages(r.messages[job.upto:upto], upto); len(since) > len(delta) {
			delta = since
		}
	}
	return delta
}
