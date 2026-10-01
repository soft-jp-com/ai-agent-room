package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupBranch は発言3件の会話を作り、「新しい会話」で別のログに移ったあと、元のログ名と発言を返す
func setupBranch(t *testing.T) (*Room, *Agent, string, []Message) {
	t.Helper()
	r, a := newTestRoom(t)
	m1 := postChat(r, "human", "A と B どちらにする？")
	m2 := postChat(r, "x", "A がよいです\n```cmd\necho 1\n```")
	m3 := postChat(r, "human", "やはり B で")
	r.mu.Lock()
	name := filepath.Base(r.logFile)
	a.sessionID = "sess-old"
	r.mu.Unlock()
	r.Reset()
	postChat(r, "human", "別の話")
	return r, a, name, []Message{m1, m2, m3}
}

// 過去ログの途中の発言までを引き継いで新しい会話を始める。元のログは変わらない（案3）
func TestBranchFromLog(t *testing.T) {
	r, a, name, src := setupBranch(t)
	before, err := os.ReadFile(filepath.Join(r.logDir, name))
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	prevLog := r.logFile
	r.mu.Unlock()

	if err := r.BranchFromLog(name, src[1].ID); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logFile == prevLog || filepath.Base(r.logFile) == name {
		t.Fatalf("新しいログファイルになっていない: %s", r.logFile)
	}
	if len(r.messages) != 3 || r.messages[0].ID != src[0].ID || r.messages[1].ID != src[1].ID ||
		r.messages[1].Text != src[1].Text || !strings.Contains(r.messages[2].Text, "#2 までを引き継いで") {
		t.Fatalf("引き継いだ発言: %+v", r.messages)
	}
	if len(r.messages[1].Blocks) != 0 {
		t.Fatal("引き継いだ発言のブロックが実行できる")
	}
	if r.nextID != r.messages[2].ID+1 || a.sessionID != "" || a.cursor != 0 {
		t.Fatalf("nextID=%d session=%q cursor=%d", r.nextID, a.sessionID, a.cursor)
	}
	// 新しいログに書き写され、再起動後の復元に使える
	got, err := readLogFile(r.logFile)
	if err != nil || len(got) != 3 || got[1].ID != src[1].ID {
		t.Fatalf("新しいログ: %d件 %v", len(got), err)
	}
	after, _ := os.ReadFile(filepath.Join(r.logDir, name))
	if string(after) != string(before) {
		t.Fatal("元のログが変わった")
	}
}

// ログや発言が見つからなければエラーにして、今の会話はリセットしない（案3）
func TestBranchFromLogNotFound(t *testing.T) {
	r, _, name, _ := setupBranch(t)
	r.mu.Lock()
	prevLog, prevLen := r.logFile, len(r.messages)
	r.mu.Unlock()
	if err := r.BranchFromLog(name, 999); !errors.Is(err, ErrBranchPointNotFound) {
		t.Fatalf("ない発言: %v", err)
	}
	if err := r.BranchFromLog("chat-20000101-000000.jsonl", 1); !errors.Is(err, ErrLogNotFound) {
		t.Fatalf("ないログ: %v", err)
	}
	if err := r.BranchFromLog("../secret.jsonl", 1); !errors.Is(err, ErrLogNotFound) {
		t.Fatalf("不正な名前: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logFile != prevLog || len(r.messages) != prevLen {
		t.Fatal("エラーなのに今の会話をリセットした")
	}
}
