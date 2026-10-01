package main

// 「要約して新しい会話」: 長くなった会話を1人のエージェントに要約させ、新しい会話の最初に要約を置いて始め直す。
// 各 CLI の会話（セッション）を作り直すので、以降のターンで過去の全履歴を読み直すトークン消費を抑えられる。
// 元の会話のログファイルはそのまま残す。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrSummarizing   = errors.New("要約を作成中です")
	ErrNothingToSumm = errors.New("要約する発言がありません")
)

// summaryMaxChars を超える会話は、後ろ（新しい発言）から summaryMaxChars 文字だけを要約に渡す
const summaryMaxChars = 200000

const summaryInstruction = "以下は、人間と複数のAIエージェントのグループチャットの記録です。" +
	"この会話を新しい会話に引き継ぐため、次の3つの見出しで、日本語の箇条書きで簡潔にまとめてください。" +
	"ファイル名・関数名・設定値など、続きの作業に必要な具体的な情報は省略しないでください。前置きや結びの文は不要です。\n" +
	"## 決定事項\n## 未完了の作業（担当・完了条件があれば含める）\n## 注意点\n\n--- 記録 ---\n"

// summaryDeltaInstruction は前回の議事録と、その後の発言から議事録を作り直すときの指示（案10）。
// 全文を渡し直さないので要約のトークンが減り、後ろから summaryMaxChars 文字で切ったときに古い決定事項が落ちることもない
const summaryDeltaInstruction = "以下は、人間と複数のAIエージェントのグループチャットの前回の議事録と、その後の新しい発言の記録です。" +
	"この会話を新しい会話に引き継ぐため、前回の議事録に新しい発言の内容を反映した議事録を、次の3つの見出しで、日本語の箇条書きで簡潔に作成してください。" +
	"「決定事項」は前回の項目を省略せずにすべて残し、新しい決定を後ろに追加してください（取り消し・変更された項目は消さずに、その旨を書き添える）。" +
	"「未完了の作業」と「注意点」は、新しい発言を踏まえて更新してください（完了した作業は除く）。" +
	"ファイル名・関数名・設定値など、続きの作業に必要な具体的な情報は省略しないでください。前置きや結びの文は不要です。\n" +
	"## 決定事項\n## 未完了の作業（担当・完了条件があれば含める）\n## 注意点\n\n--- 前回の議事録 ---\n"

const summaryDeltaSeparator = "\n\n--- 前回の議事録の後の新しい記録 ---\n"

// summarizerLocked は要約を担当するエージェントを選ぶ。進行役が参加できればその人、いなければ最初の参加可能なエージェント。
func (r *Room) summarizerLocked() *Agent {
	if a := r.agent(r.leader); a != nil && a.active() {
		return a
	}
	for _, a := range r.agents {
		if a.active() {
			return a
		}
	}
	return nil
}

// transcriptLocked は要約に渡す会話の記録を作る（パスは除く）
func (r *Room) transcriptLocked() string {
	return transcriptOf(r.messages)
}

// transcriptOf は発言の一覧を要約用の記録にする（パスは除く）。summaryMaxChars を超えたら後ろを残す
func transcriptOf(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Kind == "pass" {
			continue
		}
		fmt.Fprintf(&b, "[#%d %s %s]: %s\n\n", m.ID, m.Time, m.From, m.Text)
	}
	s := b.String()
	if rs := []rune(s); len(rs) > summaryMaxChars {
		s = "…(前半省略)…\n" + string(rs[len(rs)-summaryMaxChars:])
	}
	return s
}

// minutesPromptLocked は議事録（要約）を作るプロンプトを返す。
// 今の会話で作った議事録があれば、その内容と、それより後の発言から作る（差分）。
// 前回の議事録がない・読めない・その後の発言がない場合は、今までどおり会話全体から作る。空なら要約する発言がない
func (r *Room) minutesPromptLocked() (prompt, mode string) {
	if prev := r.lastMinutes; prev != nil && prev.upto <= len(r.messages) {
		delta := transcriptOf(r.messages[prev.upto:])
		b, err := os.ReadFile(prev.path)
		switch {
		case err != nil:
			r.log.Warn("minutes.delta", "path", prev.path, "error", err.Error())
		case strings.TrimSpace(string(b)) != "" && strings.TrimSpace(delta) != "":
			return summaryDeltaInstruction + strings.TrimSpace(string(b)) + summaryDeltaSeparator + delta, "delta"
		}
	}
	transcript := r.transcriptLocked()
	if strings.TrimSpace(transcript) == "" {
		return "", "full"
	}
	return summaryInstruction + transcript, "full"
}

// SummarizeAndReset は実行中の処理を止め、会話の要約を作ってから新しい会話を始める。
// 要約の作成には時間がかかるため裏で行い、結果（または失敗）はシステムメッセージで知らせる。
func (r *Room) SummarizeAndReset() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.summarizing {
		return ErrSummarizing
	}
	a := r.summarizerLocked()
	if a == nil {
		return ErrNoAgents
	}
	prompt, mode := r.minutesPromptLocked()
	if prompt == "" {
		return ErrNothingToSumm
	}
	r.stopLocked()
	r.summarizing = true
	oldLog := filepath.Base(r.logFile)
	gen, workdir, modelSel := r.gen, r.workdir, a.modelSel
	r.postLocked("system", fmt.Sprintf("%s が会話の要約を作成しています。完了すると新しい会話を始めます。", a.Name), "system")
	r.log.Info("conversation.summarize.start", "agent", a.ID, "log_file", oldLog, "messages", len(r.messages), "mode", mode)
	go r.finishSummary(a, prompt, oldLog, gen, workdir, modelSel)
	return nil
}

func (r *Room) finishSummary(a *Agent, prompt, oldLog string, gen int, workdir, modelSel string) {
	start := time.Now()
	// 要約は既存の会話を引き継がず、新しいセッションで作る
	res, err := a.Adapter.Run(context.Background(), prompt, "", modelSel, workdir)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.summarizing = false
	a.usage.add(res.Usage)
	r.saveUsageLocked()
	log := r.log.With("agent", a.ID, "log_file", oldLog, "latency_ms", time.Since(start).Milliseconds())
	if gen != r.gen { // 要約中に「新しい会話」などで会話が変わった
		log.Info("conversation.summarize.end", "discarded", "reset")
		return
	}
	summary := strings.TrimSpace(cleanReply(a, res.Text))
	if err == nil && summary == "" {
		err = errors.New("要約が空でした")
	}
	if err != nil {
		log.Error("conversation.summarize.end", "error", err.Error())
		r.postLocked("system", fmt.Sprintf("会話の要約に失敗しました（%s）。会話はそのまま続けられます。", err.Error()), "system")
		r.pushStatusLocked()
		return
	}
	log.Info("conversation.summarize.end", "summary_chars", len([]rune(summary)))
	r.resetLocked()
	r.postProfileLocked()
	r.postLocked("system", fmt.Sprintf("前の会話（%s）の要約です（作成: %s）。これを踏まえて続けてください。\n\n%s", oldLog, a.Name, summary), "system")
}
