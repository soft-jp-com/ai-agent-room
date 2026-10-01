package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestLiveLog(t *testing.T) {
	var l liveLog
	l.add("stdout", "a")
	backlog, ch, unsub := l.subscribe()
	defer unsub()
	if len(backlog) != 1 || backlog[0].Text != "a" {
		t.Fatalf("保持分: %+v", backlog)
	}
	l.add("stderr", strings.Repeat("x", liveMaxLineChars+5))
	ln := <-ch
	if ln.Stream != "stderr" || !strings.Contains(ln.Text, "（5文字省略）") || ln.Seq != 2 {
		t.Fatalf("配信: %+v", ln)
	}
	for i := 0; i < liveMaxLines+10; i++ {
		l.add("stdout", "y")
	}
	if b, _, u := l.subscribe(); len(b) != liveMaxLines {
		t.Fatalf("保持行数 = %d", len(b))
	} else {
		u()
	}
}

// 長い JSON の行は、中の長い文字列だけが縮み、JSON として読める形のまま残る
func TestShortenLiveLineJSON(t *testing.T) {
	long := strings.Repeat("あ", liveMaxLineChars+10)
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"a.go","content":"` + long + `"}}]}}`
	got := shortenLiveLine(line)
	if !json.Valid([]byte(got)) {
		t.Fatalf("JSON でなくなった: %.200s", got)
	}
	if !strings.HasPrefix(got, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"a.go","content":"あ`) {
		t.Fatalf("キーの順番や短い値が変わった: %.200s", got)
	}
	if !strings.Contains(got, fmt.Sprintf("…（%d文字省略）", len([]rune(long))-liveMaxStrChars)) {
		t.Fatalf("省略の表示がない: %.200s", got)
	}
	// JSON でない行は従来どおり先頭で切る
	if got := shortenLiveLine(long); !strings.HasSuffix(got, "…（10文字省略）") {
		t.Fatalf("JSON でない行: %.50s", got)
	}
	// 短い行はそのまま
	if got := shortenLiveLine(`{"a":1}`); got != `{"a":1}` {
		t.Fatalf("短い行: %s", got)
	}
}

// ターンの出力（stdout / stderr）と開始・終了が、そのエージェントの出力に入る
func TestTurnWritesLiveLog(t *testing.T) {
	r, a := newTestRoom(t)
	a.Adapter = liveAdapter{}
	selfToolURL = "http://127.0.0.1:0/mcp" // プロンプトにジョブ用トークンを入れる
	defer func() { selfToolURL = "" }()
	r.mu.Lock()
	r.postLocked("human", "hi", "chat")
	r.mu.Unlock()
	r.runTurn(context.Background(), a)
	backlog, _, unsub := r.liveFor("x").subscribe()
	defer unsub()
	var got []string
	for _, ln := range backlog {
		got = append(got, ln.Stream+":"+ln.Text)
	}
	s := strings.Join(got, "\n")
	if !strings.Contains(s, "info:=== turn-1 開始") || !strings.Contains(s, "stdout:line1") || !strings.Contains(s, "stderr:warn") || !strings.Contains(s, "info:=== turn-1 終了") {
		t.Fatalf("出力:\n%s", s)
	}
	if !strings.Contains(s, `token="`+redactedToken+`"`) || regexp.MustCompile(`[0-9a-f]{32}`).MatchString(s) {
		t.Fatalf("ジョブ用トークンが伏せられていない:\n%s", s)
	}
}

type liveAdapter struct{ fakeAdapter }

// 実際のプロセスの代わりに、runProcess と同じ Writer に出力を書く
func (liveAdapter) Run(ctx context.Context, prompt, _, _, _ string) (TurnResult, error) {
	progressWriterFrom(ctx).Write([]byte("line1\n"))
	progressWriterFrom(ctx).Write([]byte(strings.ReplaceAll(prompt, "\n", " ") + "\n")) // プロンプト（ジョブ用トークンを含む）がそのまま出力された場合
	stderrWriterFrom(ctx).Write([]byte("warn\n"))
	return TurnResult{Text: "ok"}, nil
}
