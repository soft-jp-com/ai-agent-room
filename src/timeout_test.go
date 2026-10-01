package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestClampTurnTimeout(t *testing.T) {
	// 0 以下は既定値、短すぎる・長すぎる値は下限・上限まで丸める
	cases := []struct {
		sec  int
		want time.Duration
	}{
		{0, defaultTurnTimeout},
		{-5, defaultTurnTimeout},
		{10, minTurnTimeout},
		{300, 5 * time.Minute},
		{100000, maxTurnTimeout},
		{3600, maxTurnTimeout},
	}
	for _, c := range cases {
		if got := clampTurnTimeout(c.sec); got != c.want {
			t.Errorf("clampTurnTimeout(%d) = %v, want %v", c.sec, got, c.want)
		}
	}
}

func TestTurnTimeoutFromContext(t *testing.T) {
	if got := turnTimeoutFrom(context.Background()); got != defaultTurnTimeout {
		t.Errorf("付いていないとき = %v, want %v", got, defaultTurnTimeout)
	}
	ctx := withTurnTimeout(context.Background(), 2*time.Minute)
	if got := turnTimeoutFrom(ctx); got != 2*time.Minute {
		t.Errorf("付いているとき = %v, want 2m", got)
	}
}

// エージェントごとの設定があればそれを使い、なければ会話全体の設定を使う
func TestTurnTimeoutForAgent(t *testing.T) {
	r, a := newTestRoom(t)
	if got := r.turnTimeoutForLocked(a); got != defaultTurnTimeout {
		t.Errorf("既定 = %v, want %v", got, defaultTurnTimeout)
	}
	r.SetTurnTimeout(120)
	if got := r.turnTimeoutForLocked(a); got != 2*time.Minute {
		t.Errorf("会話全体の設定 = %v, want 2m", got)
	}
	if err := r.SetAgentTimeout("x", 300); err != nil {
		t.Fatal(err)
	}
	if got := r.turnTimeoutForLocked(a); got != 5*time.Minute {
		t.Errorf("エージェントの設定 = %v, want 5m", got)
	}
	if err := r.SetAgentTimeout("x", 0); err != nil { // 0 で会話全体の設定に戻る
		t.Fatal(err)
	}
	if got := r.turnTimeoutForLocked(a); got != 2*time.Minute {
		t.Errorf("戻したあと = %v, want 2m", got)
	}
	if err := r.SetAgentTimeout("none", 60); err == nil {
		t.Error("いないエージェントを受け付けた")
	}
}

// 上限時間の設定は再起動後も残る
func TestTurnTimeoutSaveAndLoad(t *testing.T) {
	logDir := t.TempDir()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	newRoom := func() *Room {
		return NewRoom([]*Agent{{ID: "x", Name: "X", Adapter: fakeAdapter{}}}, t.TempDir(), logDir, 10, time.Second, quiet)
	}
	r := newRoom()
	r.SetTurnTimeout(180)
	if err := r.SetAgentTimeout("x", 600); err != nil {
		t.Fatal(err)
	}
	r.SaveSettings()

	r2 := newRoom()
	r2.LoadSettings(nil)
	if r2.turnTimeout != 3*time.Minute {
		t.Errorf("会話全体 = %v, want 3m", r2.turnTimeout)
	}
	if got := r2.agent("x").timeoutSec; got != 600 {
		t.Errorf("エージェント = %d, want 600", got)
	}
}

// 失敗したターンは、同じ新着を渡し直してもう一度起動できる
func TestRetryAgent(t *testing.T) {
	r, a := newTestRoom(t)
	if err := r.RetryAgent("none"); err == nil {
		t.Error("いないエージェントを受け付けた")
	}
	a.paused = true
	if err := r.RetryAgent("x"); err == nil {
		t.Error("一時停止中を受け付けた")
	}
	a.paused = false
	a.state = "thinking"
	if err := r.RetryAgent("x"); err == nil {
		t.Error("実行中を受け付けた")
	}
	a.state = "idle"
	r.mu.Lock()
	r.hops = r.maxHops // 上限に達していても、人間の操作なら起動する
	r.mu.Unlock()
	if err := r.RetryAgent("x"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	hops := r.hops
	r.mu.Unlock()
	if hops != 0 {
		t.Errorf("hops = %d, want 0", hops)
	}
	// 再試行で起動したポンプ（ゴルーチン）を止め、終わるまで待つ。
	// 待たずに抜けると、ログの書き込みが t.TempDir() の後片付けと競合する
	r.Stop()
	waitPumpStopped(t, r)
}

// waitPumpStopped はポンプのゴルーチンが終わるまで待つ
func waitPumpStopped(t *testing.T, r *Room) {
	t.Helper()
	for i := 0; i < 200; i++ {
		r.mu.Lock()
		running := r.running
		r.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ポンプが止まりません")
}
