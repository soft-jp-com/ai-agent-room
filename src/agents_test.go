package main

import (
	"errors"
	"testing"
)

// 追加・削除と、再起動（loadAgents）での復元
// fakeAgentTypes は実際の CLI（利用枠の取得など）を起動しないよう、種類ごとの生成を差し替える
func fakeAgentTypes(t *testing.T) {
	saved := agentTypes
	agentTypes = map[string]agentType{}
	for k, v := range saved {
		v.New = func() Adapter { return fakeAdapter{} }
		agentTypes[k] = v
	}
	t.Cleanup(func() { agentTypes = saved })
}

func TestAddRemoveAgent(t *testing.T) {
	fakeAgentTypes(t)
	r, _ := newTestRoom(t)
	if _, err := r.AddAgent("nope"); !errors.Is(err, ErrInvalidAgentType) {
		t.Fatalf("不正な種類: err = %v", err)
	}
	a, err := r.AddAgent("claude")
	if err != nil || a.ID != "claude2" || a.Name != "Claude Code 2" || a.Type != "claude" {
		t.Fatalf("追加: %+v %v", a, err)
	}
	if b, _ := r.AddAgent("claude"); b == nil || b.ID != "claude3" {
		t.Fatalf("2つ目の追加: %+v", b)
	}
	if ids, _ := r.mentions("@claude2 お願い", "human"); len(ids) != 1 || ids[0] != "claude2" {
		t.Fatalf("追加したエージェントを @ID で呼べない: %v", ids)
	}

	agents, err := loadAgents(r.logDir)
	if err != nil || len(agents) != 5 || agents[3].ID != "claude2" || agents[4].ID != "claude3" {
		t.Fatalf("再起動後の構成: %d件 %v", len(agents), err)
	}

	if err := r.RemoveAgent("claude2"); err != nil {
		t.Fatal(err)
	}
	if r.agent("claude2") != nil {
		t.Fatal("削除されていない")
	}
	if agents, _ := loadAgents(r.logDir); len(agents) != 4 || agents[3].ID != "claude3" {
		t.Fatalf("削除が保存されていない: %d件", len(agents))
	}

	// 会話の進行中は変更できない
	r.mu.Lock()
	r.queue = [][]string{{"x"}}
	r.mu.Unlock()
	if _, err := r.AddAgent("codex"); !errors.Is(err, ErrAgentsBusy) {
		t.Fatalf("進行中の追加: err = %v", err)
	}
}

func TestRemoveDefaultAgent(t *testing.T) {
	fakeAgentTypes(t)
	r, _ := newTestRoom(t)
	a := newAgent(agentSpec{ID: "codex", Type: "codex", Name: "Codex CLI"})
	r.agents = append(r.agents, a)
	if err := r.RemoveAgent("codex"); !errors.Is(err, ErrAgentDefault) {
		t.Fatalf("既定のエージェント: err = %v", err)
	}
	if err := r.RemoveAgent("nobody"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("存在しない: err = %v", err)
	}
}

func TestExtraIDRe(t *testing.T) {
	for id, want := range map[string]bool{"claude2": true, "claude5": true, "claude6": false, "claude1": false, "claude02": false, "claudex": false, "claude": false} {
		if got := extraIDRe("claude", id); got != want {
			t.Errorf("%s: got %v", id, got)
		}
	}
}

// 削除したエージェントの保存したモデルは、同じ ID で追加し直しても戻らない（案2 の後始末）
func TestRemoveAgentClearsSettings(t *testing.T) {
	fakeAgentTypes(t)
	r, _ := newTestRoom(t)
	if _, err := r.AddAgent("claude"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetAgentModel("claude2", "m1"); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveAgent("claude2"); err != nil {
		t.Fatal(err)
	}
	a, err := r.AddAgent("claude")
	if err != nil || a.ID != "claude2" {
		t.Fatalf("追加し直し: %+v %v", a, err)
	}
	r.LoadSettings(nil) // 再起動した想定
	if a.modelSel != "" || a.humanModel != "" {
		t.Fatalf("削除前のモデルが戻った: sel=%q human=%q", a.modelSel, a.humanModel)
	}
}
