package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type summaryAdapter struct {
	fakeAdapter
	prompt chan string
}

func (s summaryAdapter) Run(_ context.Context, prompt, sessionID, _, _ string) (TurnResult, error) {
	if sessionID != "" {
		return TurnResult{}, errors.New("要約は新しいセッションで作ること")
	}
	s.prompt <- prompt
	return TurnResult{Text: "## 決定事項\n- A にする"}, nil
}

// 要約してから新しい会話を始め、要約を最初の発言として引き継ぐ。元のログは残す
func TestSummarizeAndReset(t *testing.T) {
	r, a := newTestRoom(t)
	sa := summaryAdapter{prompt: make(chan string, 1)}
	a.Adapter = sa
	if err := r.SummarizeAndReset(); !errors.Is(err, ErrNothingToSumm) {
		t.Fatalf("空の会話: err = %v", err)
	}
	r.mu.Lock()
	r.postLocked("human", "A と B どちらにする？", "chat")
	a.sessionID = "sess-1"
	oldLog := r.logFile
	r.mu.Unlock()

	if err := r.SummarizeAndReset(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(<-sa.prompt, "A と B どちらにする？") {
		t.Fatal("要約に会話の記録が渡っていない")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		done := !r.summarizing
		r.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logFile == oldLog || a.sessionID != "" {
		t.Fatal("新しい会話になっていない")
	}
	if len(r.messages) != 1 || !strings.Contains(r.messages[0].Text, "A にする") || !strings.Contains(r.messages[0].Text, filepath.Base(oldLog)) {
		t.Fatalf("要約が引き継がれていない: %+v", r.messages)
	}
	if msgs, _ := readLogFile(oldLog); len(msgs) == 0 {
		t.Fatal("元のログが残っていない")
	}
}
