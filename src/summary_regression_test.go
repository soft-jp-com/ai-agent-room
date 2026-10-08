package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

type blockingSummaryAdapter struct {
	fakeAdapter
	started chan context.Context
	release chan struct{}
}

func (a blockingSummaryAdapter) Run(ctx context.Context, _, _, _, _ string) (TurnResult, error) {
	a.started <- ctx
	<-a.release
	return TurnResult{Text: "Summary of original messages"}, nil
}
func startBlockingSummary(t *testing.T) (*Room, *Agent, blockingSummaryAdapter) {
	t.Helper()
	r, a := newTestRoom(t)
	ad := blockingSummaryAdapter{started: make(chan context.Context, 4), release: make(chan struct{})}
	a.Adapter = ad
	a.permission = permReadOnly
	a.timeoutSec = 137
	r.mu.Lock()
	r.postLocked("human", "Original request", "chat")
	r.mu.Unlock()
	if err := r.SummarizeAndReset(); err != nil {
		t.Fatal(err)
	}
	return r, a, ad
}
func waitSummaryComplete(t *testing.T, r *Room) {
	t.Helper()
	end := time.Now().Add(time.Second)
	for time.Now().Before(end) {
		r.mu.Lock()
		done := !r.summarizing
		r.mu.Unlock()
		if done {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("summary did not finish")
}
func TestSummaryPreservesPermission(t *testing.T) {
	r, _, ad := startBlockingSummary(t)
	ctx := <-ad.started
	got := permissionFrom(ctx)
	timeout := turnTimeoutFrom(ctx)
	close(ad.release)
	waitSummaryComplete(t, r)
	if got != permReadOnly {
		t.Fatalf("read_only agent launched summary with permission=%q (default)", got)
	}
	if timeout != 137*time.Second {
		t.Fatalf("summary timeout=%v", timeout)
	}
}
func TestSummaryPreservesNewMessage(t *testing.T) {
	r, a, ad := startBlockingSummary(t)
	<-ad.started
	// Pause to isolate message persistence from subsequent ordinary turns.
	r.mu.Lock()
	a.paused = true
	r.mu.Unlock()
	if _, err := r.PostHuman("IMPORTANT_NEW_REQUEST"); err != nil {
		t.Fatal(err)
	}
	close(ad.release)
	waitSummaryComplete(t, r)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.messages {
		if strings.Contains(m.Text, "IMPORTANT_NEW_REQUEST") {
			return
		}
	}
	t.Fatal("message accepted while summary running was lost from new conversation")
}
func TestStopCancelsSummary(t *testing.T) {
	r, _, ad := startBlockingSummary(t)
	ctx := <-ad.started
	r.mu.Lock()
	oldLog := r.logFile
	r.mu.Unlock()
	r.Stop()
	canceled := ctx.Err() != nil
	close(ad.release)
	waitSummaryComplete(t, r)
	if !canceled {
		t.Fatal("Stop did not cancel summary context; summary continued and reset conversation")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logFile != oldLog {
		t.Fatal("canceled summary reset the conversation")
	}
}

func TestMinutesPreservesPermissionAndTimeout(t *testing.T) {
	r, a := newTestRoom(t)
	ad := blockingSummaryAdapter{started: make(chan context.Context, 1), release: make(chan struct{})}
	a.Adapter = ad
	a.permission = permWorkspace
	a.timeoutSec = 143
	r.mu.Lock()
	r.postLocked("human", "Original request", "chat")
	a.sessionID = "old-session"
	a.lastInput = 600000
	r.checkRotateLocked()
	job := r.minutes
	r.mu.Unlock()
	if job == nil {
		t.Fatal("minutes not started")
	}
	ctx := <-ad.started
	// A later settings change must not change an already launched job.
	r.mu.Lock()
	a.permission = permReadOnly
	a.timeoutSec = 155
	r.mu.Unlock()
	got, timeout := permissionFrom(ctx), turnTimeoutFrom(ctx)
	close(ad.release)
	select {
	case <-job.done:
	case <-time.After(time.Second):
		t.Fatal("minutes did not finish")
	}
	if got != permWorkspace || timeout != 143*time.Second {
		t.Fatalf("minutes permission=%q timeout=%v", got, timeout)
	}
	if job.err != nil {
		t.Fatal(job.err)
	}
}
