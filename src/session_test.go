package main

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestRestoreSession(t *testing.T) {
	wd, logDir := t.TempDir(), t.TempDir()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	newRoom := func(wd string) (*Room, *Agent) {
		a := &Agent{ID: "x", Name: "X", Adapter: fakeAdapter{}}
		return NewRoom([]*Agent{a}, wd, logDir, 10, time.Second, discard), a
	}

	r1, a1 := newRoom(wd)
	r1.mu.Lock()
	r1.postLocked("human", "こんにちは", "chat")
	r1.postLocked("x", "はい", "chat")
	a1.sessionID, a1.cursor = "sess-1", 2
	r1.saveSessionLocked()
	logFile := r1.logFile
	r1.mu.Unlock()

	// 再起動: 発言・会話ID・cursor を復元し、同じログへ追記を続ける
	r2, a2 := newRoom(wd)
	if len(r2.messages) != 2 || r2.messages[0].Text != "こんにちは" {
		t.Fatalf("発言が復元されていない: %+v", r2.messages)
	}
	if a2.sessionID != "sess-1" || a2.cursor != 2 {
		t.Fatalf("会話IDまたは cursor が復元されていない: %q %d", a2.sessionID, a2.cursor)
	}
	if r2.logFile != logFile {
		t.Fatalf("ログファイル = %s, want %s", r2.logFile, logFile)
	}
	r2.mu.Lock()
	if m := r2.postLocked("human", "続き", "chat"); m.ID != 3 {
		t.Fatalf("発言IDが続いていない: %d", m.ID)
	}
	r2.mu.Unlock()
	if msgs, _ := readLogFile(logFile); len(msgs) != 3 {
		t.Fatalf("同じログに追記されていない: %d件", len(msgs))
	}

	// 作業ディレクトリが変わったら復元しない
	if r3, a3 := newRoom(t.TempDir()); len(r3.messages) != 0 || a3.sessionID != "" {
		t.Fatal("作業ディレクトリが違うのに復元された")
	}

	// 「新しい会話」のあとは復元しない（同じ秒に押しても前のログと別のファイルになる）
	r2.Reset()
	r4, a4 := newRoom(wd)
	if len(r4.messages) != 0 || a4.sessionID != "" || filepath.Base(r4.logFile) == filepath.Base(logFile) {
		t.Fatal("「新しい会話」のあとに前の会話が復元された")
	}
}

// 最初のターンが終わる前に終了しても、人間の発言は復元される
func TestRestoreBeforeFirstTurn(t *testing.T) {
	wd, logDir := t.TempDir(), t.TempDir()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	r1 := NewRoom([]*Agent{{ID: "x", Name: "X", Adapter: fakeAdapter{}}}, wd, logDir, 10, time.Second, discard)
	r1.mu.Lock()
	r1.postLocked("human", "最初の依頼", "chat")
	r1.mu.Unlock()

	r2 := NewRoom([]*Agent{{ID: "x", Name: "X", Adapter: fakeAdapter{}}}, wd, logDir, 10, time.Second, discard)
	if len(r2.messages) != 1 || r2.messages[0].Text != "最初の依頼" {
		t.Fatalf("人間の発言が復元されていない: %+v", r2.messages)
	}
}

// 作業ディレクトリの変更: 新しい会話になり、再起動後も変更後のディレクトリを使う
func TestSetWorkdir(t *testing.T) {
	wd, wd2, logDir := t.TempDir(), t.TempDir(), t.TempDir()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Agent{ID: "x", Name: "X", Adapter: fakeAdapter{}}
	r := NewRoom([]*Agent{a}, wd, logDir, 10, time.Second, discard)
	r.mu.Lock()
	r.postLocked("human", "こんにちは", "chat")
	a.sessionID = "sess-1"
	r.mu.Unlock()

	if err := r.SetWorkdir(filepath.Join(wd, "missing")); !errors.Is(err, ErrInvalidWorkdir) {
		t.Fatalf("存在しないディレクトリ: err = %v", err)
	}
	if r.workdir != wd || a.sessionID != "sess-1" {
		t.Fatal("失敗したのに作業ディレクトリか会話が変わった")
	}
	if err := r.SetWorkdir(wd2); err != nil {
		t.Fatal(err)
	}
	if r.workdir != wd2 || a.sessionID != "" || len(r.messages) != 1 || r.messages[0].From != "system" {
		t.Fatalf("新しい会話になっていない: workdir=%s session=%q messages=%+v", r.workdir, a.sessionID, r.messages)
	}
	if got := savedWorkdir(logDir); got != wd2 {
		t.Fatalf("savedWorkdir = %q, want %q", got, wd2)
	}
}
