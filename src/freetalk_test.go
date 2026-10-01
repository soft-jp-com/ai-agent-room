package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// orderAdapter はターンの開始と終了の順番を記録する。最初の返答だけ発言し、以降はパスする
type orderAdapter struct {
	fakeAdapter
	id   string
	mu   *sync.Mutex
	log  *[]string
	wait time.Duration
}

func (o orderAdapter) Run(ctx context.Context, _, _, _, _ string) (TurnResult, error) {
	o.mu.Lock()
	*o.log = append(*o.log, "start:"+o.id)
	n := 0
	for _, e := range *o.log {
		if e == "start:"+o.id {
			n++
		}
	}
	o.mu.Unlock()
	time.Sleep(o.wait)
	o.mu.Lock()
	*o.log = append(*o.log, "end:"+o.id)
	o.mu.Unlock()
	if n == 1 {
		return TurnResult{Text: o.id + " の回答"}, nil
	}
	return TurnResult{Text: "[pass]"}, nil
}

func newFreeTalkRoom(t *testing.T, leader string) (*Room, *[]string, *sync.Mutex) {
	var mu sync.Mutex
	var log []string
	var agents []*Agent
	for _, id := range []string{"x", "y", "z"} {
		agents = append(agents, &Agent{ID: id, Name: id, Aliases: []string{id}, Adapter: orderAdapter{id: id, mu: &mu, log: &log, wait: 200 * time.Millisecond}})
	}
	r := NewRoom(agents, t.TempDir(), t.TempDir(), 10, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.leader = leader
	if err := r.StartFreeTalk(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	return r, &log, &mu
}

// waitStarts は、指定したエージェントがすべてターンを始めるまで待ち、記録を返す
func waitStarts(t *testing.T, log *[]string, mu *sync.Mutex, ids ...string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := append([]string{}, (*log)...)
		mu.Unlock()
		ok := true
		for _, id := range ids {
			if indexOf(got, "start:"+id) < 0 {
				ok = false
			}
		}
		if ok {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("ターンが始まらない: %v", *log)
	return nil
}

func indexOf(s []string, v string) int {
	for i, e := range s {
		if e == v {
			return i
		}
	}
	return -1
}

// フリートーク中の人間の発言には、進行役が先に答え、ほかのエージェントはその回答が終わってから考え始める
func TestFreeTalkLeaderAnswersFirst(t *testing.T) {
	r, log, mu := newFreeTalkRoom(t, "x")
	if _, err := r.PostHuman("質問です"); err != nil {
		t.Fatal(err)
	}
	got := waitStarts(t, log, mu, "x", "y", "z")
	leaderEnd := indexOf(got, "end:x")
	for _, id := range []string{"y", "z"} {
		if s := indexOf(got, "start:"+id); leaderEnd < 0 || s < leaderEnd {
			t.Fatalf("%s が進行役の回答より先に考え始めた: %v", id, got)
		}
	}
}

// 名指しされた人がいれば、進行役ではなくその人が先に答える
func TestFreeTalkMentionedAnswersFirst(t *testing.T) {
	r, log, mu := newFreeTalkRoom(t, "x")
	if _, err := r.PostHuman("@z に質問です"); err != nil {
		t.Fatal(err)
	}
	got := waitStarts(t, log, mu, "x", "y", "z")
	zEnd := indexOf(got, "end:z")
	for _, id := range []string{"x", "y"} {
		if s := indexOf(got, "start:"+id); zEnd < 0 || s < zEnd {
			t.Fatalf("%s が名指しされた人より先に考え始めた: %v", id, got)
		}
	}
}

// 進行役も名指しもなければ、全員が同時に考え始める（待たない）
func TestFreeTalkNoLeaderNoWait(t *testing.T) {
	r, log, mu := newFreeTalkRoom(t, "")
	if _, err := r.PostHuman("質問です"); err != nil {
		t.Fatal(err)
	}
	got := waitStarts(t, log, mu, "x", "y", "z")
	if first := indexOf(got, "end:x"); first >= 0 && first < 3 {
		t.Fatalf("ほかのエージェントが待たされた: %v", got)
	}
}

// 先に答える人が一時停止していれば、待たない
func TestFreeTalkFirstResponderPaused(t *testing.T) {
	r, log, mu := newFreeTalkRoom(t, "x")
	r.mu.Lock()
	r.agent("x").paused = true
	r.mu.Unlock()
	if _, err := r.PostHuman("質問です"); err != nil {
		t.Fatal(err)
	}
	waitStarts(t, log, mu, "y", "z")
}
