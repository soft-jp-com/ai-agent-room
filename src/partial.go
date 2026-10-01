package main

// 返答本文の途中表示（強化案 5）。
// CLI の JSON ストリームから書きかけの本文を取り出し、画面にだけ配信する。途中の本文は会話（r.messages）・チャットログ・
// セッション・他のエージェントへの新着のどれにも入れない。確定した発言はこれまでどおりプロセス終了後に出力全体から作る。
// Claude は --include-partial-messages の text_delta を積み上げる。Codex は本文を少しずつは出さないので、返答（agent_message）1件ごと。
// Antigravity は未確認のため対象外。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Partial は書きかけの本文。Text が空なら、そのエージェントの途中表示を消す
type Partial struct {
	Agent string `json:"agent"`
	Text  string `json:"text"`
}

type partialParserKey struct{}
type partialSinkKey struct{}

// withPartialParser は出力1行から書きかけの本文全体を取り出す関数を ctx に付ける（各アダプタが Run ごとに作って設定する）。
// 本文が変わらない行では ok=false を返す
func withPartialParser(ctx context.Context, parse func(line string) (text string, ok bool)) context.Context {
	return context.WithValue(ctx, partialParserKey{}, parse)
}

// withPartialSink は書きかけの本文の通知先を ctx に付ける（Room が設定する）
func withPartialSink(ctx context.Context, sink func(text string)) context.Context {
	return context.WithValue(ctx, partialSinkKey{}, sink)
}

// partialFrom は ctx に付いた parser と通知先を1つにした関数を返す。どちらかがなければ nil
func partialFrom(ctx context.Context) func(line string) {
	parse, _ := ctx.Value(partialParserKey{}).(func(string) (string, bool))
	sink, _ := ctx.Value(partialSinkKey{}).(func(string))
	if parse == nil || sink == nil {
		return nil
	}
	return func(line string) {
		if s, ok := parse(line); ok {
			sink(s)
		}
	}
}

// newClaudePartial は stream-json（--include-partial-messages）の text_delta を積み上げる parser を返す。
// メッセージの始まり（message_start）で空にする。thinking やサブエージェント（parent_tool_use_id あり）の本文は使わない
func newClaudePartial() func(string) (string, bool) {
	var b strings.Builder
	return func(line string) (string, bool) {
		if !strings.Contains(line, `"stream_event"`) {
			return "", false
		}
		var ev struct {
			Type            string  `json:"type"`
			ParentToolUseID *string `json:"parent_tool_use_id"`
			Event           struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			} `json:"event"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type != "stream_event" || ev.ParentToolUseID != nil {
			return "", false
		}
		switch {
		case ev.Event.Type == "message_start":
			if b.Len() == 0 {
				return "", false
			}
			b.Reset()
			return "", true // 前のメッセージ（ツール呼び出しの前の本文）の表示を消す
		case ev.Event.Type == "content_block_delta" && ev.Event.Delta.Type == "text_delta":
			b.WriteString(ev.Event.Delta.Text)
			return b.String(), true
		}
		return "", false
	}
}

// codexPartial は codex exec --json の返答（agent_message）が1件届くたびに、その本文を返す
func codexPartial(line string) (string, bool) {
	if !strings.Contains(line, `"agent_message"`) {
		return "", false
	}
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil || ev.Type != "item.completed" || ev.Item.Type != "agent_message" {
		return "", false
	}
	return ev.Item.Text, true
}

// partialInterval より短い間隔の途中表示は間引く（最後の本文は確定した発言で置き換わる）
const partialInterval = 300 * time.Millisecond

// partialThrottle は途中表示の配信を間引く。空の本文（表示を消す）は間引かない。
// 間引いた本文は、間隔が空いたときに最新のものを送る（送らないと、短い本文が最初の断片のまま残る）
type partialThrottle struct {
	mu      sync.Mutex
	last    time.Time
	pending string
	timer   *time.Timer
}

func (p *partialThrottle) push(text string, send func(string)) {
	p.mu.Lock()
	if text == "" {
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		p.pending, p.last = "", time.Now()
		p.mu.Unlock()
		send("")
		return
	}
	if wait := partialInterval - time.Since(p.last); wait > 0 || p.timer != nil {
		p.pending = text
		if p.timer == nil {
			p.timer = time.AfterFunc(max(wait, 0), p.flush(send))
		}
		p.mu.Unlock()
		return
	}
	p.last = time.Now()
	p.mu.Unlock()
	send(text)
}

func (p *partialThrottle) flush(send func(string)) func() {
	return func() {
		p.mu.Lock()
		text := p.pending
		p.pending, p.timer, p.last = "", nil, time.Now()
		p.mu.Unlock()
		if text != "" {
			send(text)
		}
	}
}
