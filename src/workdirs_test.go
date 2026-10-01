package main

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// 作業ディレクトリを変えて戻ると、そのディレクトリの前回の会話（発言・会話ID・cursor）が戻る。一時停止は引き継がない
func TestSetWorkdirRestoresConversation(t *testing.T) {
	wd, wd2, logDir := t.TempDir(), t.TempDir(), t.TempDir()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Agent{ID: "x", Name: "X", Adapter: fakeAdapter{}}
	r := NewRoom([]*Agent{a}, wd, logDir, 10, time.Second, discard)
	r.mu.Lock()
	r.postLocked("human", "wd の会話", "chat")
	a.sessionID, a.cursor = "sess-wd", 1
	r.mu.Unlock()

	if err := r.SetWorkdir(wd2); err != nil {
		t.Fatal(err)
	}
	if a.sessionID != "" || len(r.messages) != 1 || !strings.Contains(r.messages[0].Text, "新しい会話") {
		t.Fatalf("初めてのディレクトリは新しい会話のはず: session=%q messages=%+v", a.sessionID, r.messages)
	}
	r.mu.Lock()
	r.postLocked("human", "wd2 の会話", "chat")
	a.sessionID, a.cursor = "sess-wd2", 2
	a.paused = true
	r.mu.Unlock()

	if err := r.SetWorkdir(wd); err != nil {
		t.Fatal(err)
	}
	if a.sessionID != "sess-wd" || a.cursor != 1 || !a.paused {
		t.Fatalf("会話IDか cursor が戻っていない、または一時停止が変わった: session=%q cursor=%d paused=%v", a.sessionID, a.cursor, a.paused)
	}
	if len(r.messages) != 2 || r.messages[0].Text != "wd の会話" || !strings.Contains(r.messages[1].Text, "前回の会話を復元") {
		t.Fatalf("発言が戻っていない: %+v", r.messages)
	}
	if r.nextID != r.messages[1].ID+1 {
		t.Fatalf("nextID = %d", r.nextID)
	}

	// もう一度 wd2 に戻ると、wd2 の会話が戻る
	if err := r.SetWorkdir(wd2); err != nil {
		t.Fatal(err)
	}
	if a.sessionID != "sess-wd2" || len(r.messages) != 3 || r.messages[1].Text != "wd2 の会話" {
		t.Fatalf("wd2 の会話が戻っていない: session=%q messages=%+v", a.sessionID, r.messages)
	}
	// 再起動しても今の会話（wd2）が復元される
	r2 := NewRoom([]*Agent{{ID: "x", Name: "X", Adapter: fakeAdapter{}}}, wd2, logDir, 10, time.Second, discard)
	if len(r2.messages) != 3 || r2.agents[0].sessionID != "sess-wd2" {
		t.Fatalf("再起動後: %+v", r2.messages)
	}
}

// 別の作業ディレクトリで起動すると、前回の会話はそのディレクトリの会話として残り、戻って起動すると復元される。
// 作業ディレクトリの大文字・小文字の違いは同じディレクトリとして扱う
func TestStartupWorkdirChangeArchives(t *testing.T) {
	wd, wd2, logDir := t.TempDir(), t.TempDir(), t.TempDir()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	newAgent := func() *Agent { return &Agent{ID: "x", Name: "X", Adapter: fakeAdapter{}} }
	r := NewRoom([]*Agent{newAgent()}, wd, logDir, 10, time.Second, discard)
	r.mu.Lock()
	r.postLocked("human", "wd の会話", "chat")
	r.agents[0].sessionID = "sess-wd"
	r.saveSessionLocked()
	r.mu.Unlock()

	r2 := NewRoom([]*Agent{newAgent()}, wd2, logDir, 10, time.Second, discard)
	if len(r2.messages) != 0 || r2.agents[0].sessionID != "" {
		t.Fatalf("wd2 は新しい会話のはず: %+v", r2.messages)
	}
	r3 := NewRoom([]*Agent{newAgent()}, strings.ToUpper(wd), logDir, 10, time.Second, discard)
	if len(r3.messages) != 1 || r3.messages[0].Text != "wd の会話" || r3.agents[0].sessionID != "sess-wd" {
		t.Fatalf("wd の会話が戻っていない: session=%q messages=%+v", r3.agents[0].sessionID, r3.messages)
	}
}

// 残す作業ディレクトリが上限を超えたら、古いものから消す
func TestArchiveWorkdirLimit(t *testing.T) {
	r, _ := newTestRoom(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range workdirsMax + 3 {
		r.archiveSessionLocked(sessionFile{LogFile: "chat-20260925-000000.jsonl", Workdir: fmt.Sprintf(`C:\w%02d`, i)})
		time.Sleep(time.Millisecond) // 保存時刻に差をつける
	}
	m := r.loadWorkdirsLocked()
	if len(m) != workdirsMax {
		t.Fatalf("件数 = %d", len(m))
	}
	for _, gone := range []string{`C:\w00`, `C:\w01`, `C:\w02`} {
		if _, ok := m[gone]; ok {
			t.Errorf("古い %s が残っている", gone)
		}
	}
	if _, ok := m[fmt.Sprintf(`C:\w%02d`, workdirsMax+2)]; !ok {
		t.Error("新しいものが消えた")
	}
}
