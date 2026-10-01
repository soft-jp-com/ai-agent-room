package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLeaseAndRelease(t *testing.T) {
	r, _ := newTestRoom(t)
	r.jobs["tx"] = &jobTicket{agentID: "x", gen: r.gen}
	r.jobs["ty"] = &jobTicket{agentID: "y", gen: r.gen}

	if _, err := r.leaseByAgent("bad", "cdp-9333", 0); err == nil {
		t.Fatal("無効なトークンで借りられた")
	}
	if _, err := r.leaseByAgent("tx", " ", 0); err == nil {
		t.Fatal("空の名前で借りられた")
	}
	if _, err := r.leaseByAgent("tx", "CDP-9333", 0); err != nil {
		t.Fatal(err)
	}
	// 大文字小文字を区別せず、ほかのエージェントは借りられない
	if _, err := r.leaseByAgent("ty", "cdp-9333", 0); err == nil || !strings.Contains(err.Error(), "@x") {
		t.Fatalf("使用中なのに借りられた: %v", err)
	}
	// ほかのエージェントは返せない
	if _, err := r.releaseByAgent("ty", "cdp-9333"); err == nil {
		t.Fatal("借りていない人が返せた")
	}
	r.mu.Lock()
	ls := r.leasesLocked()
	st := r.statusLocked()
	r.mu.Unlock()
	if len(ls) != 1 || ls[0].Holder != "x" || len(st.Leases) != 1 {
		t.Fatalf("貸し出しの一覧 %#v / status %#v", ls, st.Leases)
	}
	// 既定の期限は30分
	if d := time.Until(time.UnixMilli(ls[0].Until)); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("期限 %v", d)
	}
	if _, err := r.releaseByAgent("tx", "cdp-9333"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.leaseByAgent("ty", "cdp-9333", 1000); err != nil {
		t.Fatalf("返却後に借りられない: %v", err)
	}
	r.mu.Lock()
	until := r.leases["cdp-9333"].Until
	r.mu.Unlock()
	if d := time.Until(time.UnixMilli(until)); d > leaseMaxMinutes*time.Minute+time.Minute {
		t.Fatalf("上限を超えて借りられた: %v", d)
	}
}

// 期限を過ぎた貸し出しは、ほかのエージェントが借りられる
func TestLeaseExpires(t *testing.T) {
	r, _ := newTestRoom(t)
	r.jobs["tx"] = &jobTicket{agentID: "x", gen: r.gen}
	r.jobs["ty"] = &jobTicket{agentID: "y", gen: r.gen}
	if _, err := r.leaseByAgent("tx", "cdp-9333", 0); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.leases["cdp-9333"].Until = time.Now().Add(-time.Second).UnixMilli()
	r.mu.Unlock()
	if _, err := r.leaseByAgent("ty", "cdp-9333", 0); err != nil {
		t.Fatalf("期限切れなのに借りられない: %v", err)
	}
}

// MCP の tools/list に3つのツールが載り、tools/call で貸し出しを呼べる
func TestLeaseViaMCP(t *testing.T) {
	r, _ := newTestRoom(t)
	r.jobs["tx"] = &jobTicket{agentID: "x", gen: r.gen}
	call := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		r.handleMCP(rec, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body)))
		var res map[string]any
		json.Unmarshal(rec.Body.Bytes(), &res)
		return res
	}
	list := call(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != strings.Join(selfToolNames, ",") {
		t.Fatalf("ツールの一覧 %v", names)
	}
	res := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"lease_resource","arguments":{"token":"tx","name":"cdp-9333"}}}`)
	if res["result"].(map[string]any)["isError"] != false {
		t.Fatalf("借りられない: %v", res)
	}
	if got := claudeAllowedTools(); got != "mcp__ai_agent_room__set_my_model,mcp__ai_agent_room__lease_resource,mcp__ai_agent_room__release_resource" {
		t.Fatalf("allowedTools = %q", got)
	}
}
