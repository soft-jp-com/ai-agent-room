package main

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestSettingsSaveAndLoad(t *testing.T) {
	logDir := t.TempDir()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	newRoom := func() *Room {
		return NewRoom([]*Agent{{ID: "x", Name: "X", Adapter: fakeAdapter{}}}, t.TempDir(), logDir, 10, 3*time.Second, quiet)
	}

	r := newRoom()
	r.SetMaxHops(50)
	r.SetDelay(9)
	r.SetRotateTokens(300000)
	if err := r.SetLeader("x"); err != nil {
		t.Fatal(err)
	}
	r.SaveSettings()

	// 再起動した想定：保存した設定が戻る
	r2 := newRoom()
	r2.LoadSettings(nil)
	if r2.maxHops != 50 || r2.delay != 9*time.Second || r2.rotateTokens != 300000 || r2.leader != "x" {
		t.Fatalf("maxHops=%d delay=%v rotate=%d leader=%q", r2.maxHops, r2.delay, r2.rotateTokens, r2.leader)
	}

	// コマンドラインで指定した項目は、保存した値で上書きしない
	r3 := newRoom()
	r3.LoadSettings(map[string]bool{"max_hops": true, "delay_sec": true})
	if r3.maxHops != 10 || r3.delay != 3*time.Second || r3.rotateTokens != 300000 {
		t.Fatalf("maxHops=%d delay=%v rotate=%d", r3.maxHops, r3.delay, r3.rotateTokens)
	}
}

func TestSettingsLoadMissingLeader(t *testing.T) {
	r, _ := newTestRoom(t)
	if err := writeStateFile(r.settingsPath(), []byte(`{"max_hops": 20, "leader": "gone"}`)); err != nil {
		t.Fatal(err)
	}
	r.LoadSettings(nil)
	if r.maxHops != 20 || r.leader != "" {
		t.Fatalf("maxHops=%d leader=%q", r.maxHops, r.leader)
	}
}

// 人間が選んだモデルは保存して再起動後に戻す。エージェントが自分で変えたモデルは保存しない（案2）
func TestSettingsAgentModel(t *testing.T) {
	logDir := t.TempDir()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	newRoom := func() (*Room, *Agent) {
		a := &Agent{ID: "x", Name: "X", Adapter: fakeAdapter{}}
		return NewRoom([]*Agent{a}, t.TempDir(), logDir, 10, 3*time.Second, quiet), a
	}

	r, a := newRoom()
	if err := r.SetAgentModel("x", "m1"); err != nil { // 保存は SetAgentModel が行う
		t.Fatal(err)
	}
	r.mu.Lock()
	r.applyModelLocked(a, "m2", "agent") // エージェント本人の切り替え（set_my_model）
	r.mu.Unlock()
	r.SaveSettings()

	r2, a2 := newRoom()
	r2.LoadSettings(nil)
	if a2.modelSel != "m1" || a2.humanModel != "m1" {
		t.Fatalf("人間の選択が戻らない: sel=%q human=%q", a2.modelSel, a2.humanModel)
	}

	// 既定に戻したら、保存からも消える
	if err := r2.SetAgentModel("x", ""); err != nil {
		t.Fatal(err)
	}
	r3, a3 := newRoom()
	r3.LoadSettings(nil)
	if a3.modelSel != "" {
		t.Fatalf("既定に戻したのに %q が戻った", a3.modelSel)
	}
}

// 消えたエージェントと候補にないモデルは戻さない（案2）
func TestSettingsAgentModelInvalid(t *testing.T) {
	r, a := newTestRoom(t)
	if err := writeStateFile(r.settingsPath(), []byte(`{"max_hops": 20, "agent_model": {"x": "zzz", "gone": "m1"}}`)); err != nil {
		t.Fatal(err)
	}
	r.LoadSettings(nil)
	if r.maxHops != 20 || a.modelSel != "" || a.humanModel != "" {
		t.Fatalf("maxHops=%d sel=%q human=%q", r.maxHops, a.modelSel, a.humanModel)
	}
}

// 上限は 0 で無制限。100 を超える値も設定でき、負の値は 1 にする
func TestMaxHopsUnlimited(t *testing.T) {
	r := NewRoom([]*Agent{{ID: "x", Name: "X", Adapter: fakeAdapter{}}}, t.TempDir(), t.TempDir(), 100, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, c := range []struct{ in, want int }{{0, 0}, {500, 500}, {-5, 1}, {100, 100}} {
		r.SetMaxHops(c.in)
		if r.maxHops != c.want {
			t.Fatalf("SetMaxHops(%d) → %d（期待 %d）", c.in, r.maxHops, c.want)
		}
	}
	if hopLimitReached(1000, 0) {
		t.Fatal("上限 0（無制限）で止まった")
	}
	if !hopLimitReached(10, 10) || hopLimitReached(9, 10) {
		t.Fatal("上限の判定が違う")
	}
}
