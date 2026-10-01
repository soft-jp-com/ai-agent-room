package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSetLeaderAndPrompt(t *testing.T) {
	r, a := newTestRoom(t)
	if err := r.SetLeader("nobody"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("存在しないエージェント: err = %v", err)
	}
	if p := r.buildPrompt(a, nil, "tok", ""); strings.Contains(p, "進行役:") {
		t.Fatalf("未指定なのに進行役が書かれている: %q", p)
	}
	if err := r.SetLeader("x"); err != nil {
		t.Fatal(err)
	}
	if p := r.buildPrompt(a, nil, "tok", ""); !strings.Contains(p, "進行役: あなたです。") {
		t.Fatalf("本人向けの進行役の説明がない: %q", p)
	}
	b := &Agent{ID: "y", Name: "Y", Adapter: fakeAdapter{}}
	r.agents = append(r.agents, b)
	if p := r.buildPrompt(b, nil, "tok", ""); !strings.Contains(p, "進行役: @x（X）。") {
		t.Fatalf("ほかのエージェント向けの進行役の説明がない: %q", p)
	}
	if err := r.SetLeader(""); err != nil {
		t.Fatal(err)
	}
	if p := r.buildPrompt(a, nil, "tok", ""); strings.Contains(p, "進行役:") {
		t.Fatalf("解除後も進行役が書かれている: %q", p)
	}
}

// 不正な進行役を送ったときは、同時に送ったほかの設定も変えない
func TestSettingsInvalidLeaderKeepsOthers(t *testing.T) {
	r, _ := newTestRoom(t)
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"max_hops": 50, "delay_sec": 9, "leader": "nobody"}`))
	w := httptest.NewRecorder()
	r.handleSettings(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if r.maxHops != 10 || r.delay != time.Second || r.leader != "" {
		t.Fatalf("設定が変わっている: max_hops=%d delay=%v leader=%q", r.maxHops, r.delay, r.leader)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"max_hops": 50, "leader": "x"}`))
	w = httptest.NewRecorder()
	r.handleSettings(w, req)
	if w.Code != http.StatusOK || r.maxHops != 50 || r.leader != "x" {
		t.Fatalf("正しい指定が反映されない: status=%d max_hops=%d leader=%q", w.Code, r.maxHops, r.leader)
	}
}

// 作業ディレクトリが不正なら 400 を返し、ほかの設定も変えない
func TestSettingsInvalidWorkdirKeepsOthers(t *testing.T) {
	r, _ := newTestRoom(t)
	wd := r.workdir
	body, _ := json.Marshal(map[string]any{"max_hops": 50, "leader": "x", "workdir": filepath.Join(t.TempDir(), "missing")})
	w := httptest.NewRecorder()
	r.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if r.maxHops != 10 || r.leader != "" || r.workdir != wd {
		t.Fatalf("設定が変わっている: max_hops=%d leader=%q workdir=%s", r.maxHops, r.leader, r.workdir)
	}
}
