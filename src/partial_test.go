package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Claude の text_delta を積み上げ、thinking・サブエージェントの本文は使わない。message_start で空にする（案5）
func TestClaudePartial(t *testing.T) {
	p := newClaudePartial()
	lines := []string{
		`{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"考え中の秘密"}},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"こんにちは"}},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"サブの本文"}},"parent_tool_use_id":"toolu_1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"こんにちは"}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"、世界"}},"parent_tool_use_id":null}`,
	}
	var got []string
	for _, l := range lines {
		if s, ok := p(l); ok {
			got = append(got, s)
		}
	}
	if strings.Join(got, "|") != "こんにちは|こんにちは、世界" {
		t.Fatalf("got %q", got)
	}
	// 次のメッセージ（ツール呼び出しの後）が始まったら、表示を消してから積み直す
	if s, ok := p(`{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null}`); !ok || s != "" {
		t.Fatalf("message_start: %q %v", s, ok)
	}
	if s, _ := p(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"次"}},"parent_tool_use_id":null}`); s != "次" {
		t.Fatalf("積み直し: %q", s)
	}
}

func TestCodexPartial(t *testing.T) {
	if s, ok := codexPartial(`{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"返答1"}}`); !ok || s != "返答1" {
		t.Fatalf("%q %v", s, ok)
	}
	if _, ok := codexPartial(`{"type":"item.completed","item":{"type":"command_execution","command":"agent_message"}}`); ok {
		t.Fatal("agent_message 以外を本文にした")
	}
}

// partialAdapter は途中の本文を出力として流してから、確定した本文を返す
type partialAdapter struct{ fakeAdapter }

func (partialAdapter) Run(ctx context.Context, _, _, _, _ string) (TurnResult, error) {
	if w := progressWriterFrom(withPartialParser(ctx, codexPartial)); w != nil {
		w.Write([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"書きかけ"}}` + "\n"))
	}
	return TurnResult{Text: "確定した返答"}, nil
}

// 途中の本文は画面にだけ配信し、会話・ログには入れない。ターンが終わったら表示を消す（案5）
func TestPartialBroadcastNotStored(t *testing.T) {
	r, a := newTestRoom(t)
	a.Adapter = partialAdapter{}
	ch, unsub := r.Subscribe()
	defer unsub()
	r.mu.Lock()
	r.postLocked("human", "お願い", "chat")
	r.mu.Unlock()
	r.runTurn(context.Background(), a)

	var partials []string
	for len(ch) > 0 {
		if ev := <-ch; ev.Type == "partial" && ev.Partial.Agent == a.ID {
			partials = append(partials, ev.Partial.Text)
		}
	}
	if strings.Join(partials, "|") != "書きかけ|" {
		t.Fatalf("配信: %q", partials)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.messages {
		if strings.Contains(m.Text, "書きかけ") {
			t.Fatalf("途中の本文が会話に入った: %+v", m)
		}
	}
	msgs, _ := readLogFile(r.logFile)
	for _, m := range msgs {
		if strings.Contains(m.Text, "書きかけ") {
			t.Fatal("途中の本文がログに入った")
		}
	}
}

// 書きかけの本文の行は、出力の別窓には流さない（案5）
func TestStreamEventNotSentToLive(t *testing.T) {
	var lines []string
	ctx := withLiveSink(context.Background(), func(_, l string) { lines = append(lines, l) })
	w := progressWriterFrom(ctx)
	w.Write([]byte(`{"type":"stream_event","event":{"type":"content_block_delta"}}` + "\n" + `{"type":"assistant"}` + "\n"))
	if len(lines) != 1 || lines[0] != `{"type":"assistant"}` {
		t.Fatalf("別窓に流れた行: %q", lines)
	}
}

// 間引いた本文は、間隔が空いたあとに最新のものを送る。空の本文は待っている本文を捨てて、すぐに送る
func TestPartialThrottleTrailing(t *testing.T) {
	var mu sync.Mutex
	var got []string
	send := func(s string) { mu.Lock(); got = append(got, s); mu.Unlock() }
	snapshot := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }

	var p partialThrottle
	p.push("テ", send)
	p.push("テス", send)
	p.push("テスト", send)
	if g := snapshot(); len(g) != 1 || g[0] != "テ" {
		t.Fatalf("最初の断片だけがすぐ送られるはず: %q", g)
	}
	time.Sleep(partialInterval + 200*time.Millisecond)
	if g := snapshot(); len(g) != 2 || g[1] != "テスト" {
		t.Fatalf("間引いた最新の本文が後から送られていない: %q", g)
	}

	p.push("次", send) // 間隔内なので待たせる
	p.push("", send)  // 表示を消す。待っている本文は捨てる
	time.Sleep(partialInterval + 200*time.Millisecond)
	if g := snapshot(); len(g) != 3 || g[2] != "" {
		t.Fatalf("空の本文のあとに古い本文が送られた: %q", g)
	}
}
