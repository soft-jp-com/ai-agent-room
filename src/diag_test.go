package main

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// diagAdapter は --version の代わりに任意のコマンドを実行するアダプタ
type diagAdapter struct {
	fakeAdapter
	bin  string
	args []string
}

func (d diagAdapter) versionCommand() (string, []string) { return d.bin, d.args }

// 見つからない・起動に失敗・時間切れ・取得できた・非対応を区別する（案6）
func TestDiagnose(t *testing.T) {
	saved := diagTimeout
	diagTimeout = 2 * time.Second
	t.Cleanup(func() { diagTimeout = saved })

	agents := []*Agent{
		{ID: "none", Name: "None", Adapter: diagAdapter{}},
		{ID: "bad", Name: "Bad", Adapter: diagAdapter{bin: `C:\no\such\cli.exe`}},
		{ID: "fake", Name: "Fake", Adapter: fakeAdapter{}},
	}
	if runtime.GOOS == "windows" {
		agents = append(agents,
			&Agent{ID: "ok", Name: "OK", Adapter: diagAdapter{bin: "cmd", args: []string{"/c", "echo 1.2.3 (Test CLI)"}}},
			&Agent{ID: "exit", Name: "Exit", Adapter: diagAdapter{bin: "cmd", args: []string{"/c", "echo boom 1>&2 & exit /b 3"}}},
			&Agent{ID: "slow", Name: "Slow", Adapter: diagAdapter{bin: "cmd", args: []string{"/c", "ping -n 10 127.0.0.1 >nul"}}},
		)
	}
	r, _ := newTestRoom(t)
	r.mu.Lock()
	r.agents = agents
	r.mu.Unlock()

	got := map[string]DiagResult{}
	for _, d := range r.Diagnose(context.Background()) {
		got[d.Agent] = d
	}
	want := map[string]string{"none": "not_found", "bad": "failed", "fake": "unsupported"}
	if runtime.GOOS == "windows" {
		want["ok"], want["exit"], want["slow"] = "ok", "failed", "timeout"
	}
	for id, st := range want {
		if got[id].Status != st {
			t.Errorf("%s: status=%q want %q (%+v)", id, got[id].Status, st, got[id])
		}
	}
	if runtime.GOOS == "windows" {
		if got["ok"].Version != "1.2.3 (Test CLI)" || got["exit"].Detail != "boom" {
			t.Fatalf("ok=%+v exit=%+v", got["ok"], got["exit"])
		}
		if got["slow"].Ms > 8000 {
			t.Fatalf("時間切れを待ちすぎた: %dms", got["slow"].Ms)
		}
	}
	if !strings.Contains(got["none"].Detail, "見つかりません") {
		t.Fatalf("none=%+v", got["none"])
	}
}

// 実際のアダプタは --version を渡す（codex は node で JS エントリを起動する場合も）
func TestVersionCommand(t *testing.T) {
	if bin, args := (&codexAdapter{bin: "node", pre: []string{"codex.js"}}).versionCommand(); bin != "node" || strings.Join(args, " ") != "codex.js --version" {
		t.Fatalf("codex: %s %v", bin, args)
	}
	if _, args := (&claudeAdapter{bin: "claude"}).versionCommand(); strings.Join(args, " ") != "--version" {
		t.Fatalf("claude: %v", args)
	}
}
