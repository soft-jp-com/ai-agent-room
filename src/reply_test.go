package main

import (
	"errors"
	"fmt"
	"testing"
)

// 返信として投稿した発言は reply_to を持ち、エージェントへの新着に「#N への返信」と書き添える（案 8）
func TestPostHumanReply(t *testing.T) {
	r, _ := newTestRoom(t)
	first, _ := r.PostHuman("最初")
	m, err := r.PostHumanReply("返信です", first.ID)
	if err != nil || m.ReplyTo != first.ID {
		t.Fatalf("返信: %+v, %v", m, err)
	}
	if got, want := formatDelta(m), fmt.Sprintf("[#%d human（#%d への返信）]: 返信です", m.ID, first.ID); got != want {
		t.Fatalf("formatDelta = %q, want %q", got, want)
	}
	m.ReadUpTo = first.ID
	if got, want := formatDelta(m), fmt.Sprintf("[#%d human（#%d への返信、#%d まで読了）]: 返信です", m.ID, first.ID, first.ID); got != want {
		t.Fatalf("formatDelta = %q, want %q", got, want)
	}
	r.Reset() // 新しい会話の前の発言には返信できない
	for _, id := range []int{-1, m.ID + 1, first.ID} {
		if _, err := r.PostHumanReply("x", id); !errors.Is(err, ErrInvalidReply) {
			t.Fatalf("返信先 %d: err = %v", id, err)
		}
	}
}
