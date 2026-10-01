package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// 案8: 段階ごとの引数。既定は *_ARGS のまま、段階を選んだら *_ARGS の権限のオプションを外して画面の選択を優先する
func TestArgsWithPermission(t *testing.T) {
	t.Setenv("CLAUDE_ARGS", "--permission-mode bypassPermissions --dangerously-skip-permissions --verbose-x")
	t.Setenv("CODEX_ARGS", `-c sandbox_mode=danger-full-access -s danger-full-access --sandbox=workspace-write --full-auto -c model_reasoning_effort=high`)
	claude, codex := &claudeAdapter{bin: "claude"}, &codexAdapter{bin: "codex"}
	cases := []struct {
		name  string
		ad    any
		env   string
		level string
		want  []string
	}{
		{"claude 既定", claude, "CLAUDE_ARGS", permDefault, []string{"--permission-mode", "bypassPermissions", "--dangerously-skip-permissions", "--verbose-x"}},
		{"claude 読み取り", claude, "CLAUDE_ARGS", permReadOnly, []string{"--verbose-x", "--disallowedTools=Edit,Write,NotebookEdit,Bash,PowerShell", "--strict-mcp-config"}},
		{"claude 書き込み", claude, "CLAUDE_ARGS", permWorkspace, []string{"--verbose-x", "--permission-mode", "acceptEdits"}},
		{"codex 既定", codex, "CODEX_ARGS", permDefault, strings.Fields(`-c sandbox_mode=danger-full-access -s danger-full-access --sandbox=workspace-write --full-auto -c model_reasoning_effort=high`)},
		{"codex 読み取り", codex, "CODEX_ARGS", permReadOnly, []string{"-c", "model_reasoning_effort=high", "-c", `sandbox_mode="read-only"`}},
		{"codex 書き込み", codex, "CODEX_ARGS", permWorkspace, []string{"-c", "model_reasoning_effort=high", "-c", `sandbox_mode="workspace-write"`}},
	}
	for _, c := range cases {
		if got := argsWithPermission(c.ad, c.env, c.level); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// 段階を指定できない CLI（Antigravity）は *_ARGS のまま
	t.Setenv("AGY_ARGS", "--sandbox")
	if got := argsWithPermission(&agyAdapter{bin: "agy"}, "AGY_ARGS", permReadOnly); !slices.Equal(got, []string{"--sandbox"}) {
		t.Errorf("agy: %q", got)
	}
}

// 案8: どの段階でも、Claude には守るファイルへの禁止ルール（--settings の deny）を付ける。非対話・対話モードの両方
func TestPermissionKeepsDeny(t *testing.T) {
	old := protection
	t.Cleanup(func() { setProtection(old) })
	setProtection(buildProtection(t.TempDir(), t.TempDir(), ""))
	if claudeDenySettings() == "" {
		t.Fatal("テストの前提: 禁止ルールがない")
	}
	t.Setenv("CLAUDE_ARGS", "")
	ad := &claudeAdapter{bin: "claude"}
	for _, level := range []string{permDefault, permReadOnly, permWorkspace} {
		if _, args := ad.InteractiveCommand("s1", "", level); !slices.ContainsFunc(args, func(s string) bool { return strings.HasPrefix(s, "--settings=") }) {
			t.Errorf("対話モード・段階 %q で禁止ルールが外れた: %q", level, args)
		}
		if !slices.ContainsFunc(ad.runArgs("s1", "", level), func(s string) bool { return strings.HasPrefix(s, "--settings=") }) {
			t.Errorf("非対話・段階 %q で禁止ルールが外れた", level)
		}
		// 段階の引数は --settings を含まない（禁止ルールを上書きしない）
		if slices.ContainsFunc(argsWithPermission(ad, "CLAUDE_ARGS", level), func(s string) bool { return strings.HasPrefix(s, "--settings") }) {
			t.Errorf("段階 %q の引数が --settings を含む（禁止ルールを上書きしうる）", level)
		}
	}
	// 読み取りのみは、利用者側の MCP（termio など）を使わせない（--strict-mcp-config）。非対話・対話モードの両方
	_, inter := ad.InteractiveCommand("s1", "", permReadOnly)
	for name, args := range map[string][]string{"非対話": ad.runArgs("s1", "", permReadOnly), "対話モード": inter} {
		if !slices.Contains(args, "--strict-mcp-config") {
			t.Errorf("%s・読み取りのみで --strict-mcp-config がない: %q", name, args)
		}
	}
	if slices.Contains(ad.runArgs("s1", "", permWorkspace), "--strict-mcp-config") {
		t.Error("書き込み可で MCP を絞った（読み取りのみだけのはず）")
	}
}

// 案8: 人間が選んだ段階は、ターンの ctx に載り、保存して再起動後に戻る。選べない段階・CLI は断る
func TestSetAgentPermission(t *testing.T) {
	r, a := newTestRoom(t)
	r.SetConfigDir(t.TempDir())
	if err := r.SetAgentPermission("x", permReadOnly); !errors.Is(err, ErrInvalidPermission) {
		t.Fatalf("段階を指定できない CLI で受け付けた: %v", err)
	}
	r.mu.Lock()
	a.Adapter = &claudeAdapter{bin: "claude"}
	r.mu.Unlock()
	for _, bad := range []string{"full", "bypassPermissions", "READ_ONLY"} {
		if err := r.SetAgentPermission("x", bad); !errors.Is(err, ErrInvalidPermission) {
			t.Fatalf("不正な段階 %q を受け付けた: %v", bad, err)
		}
	}
	if err := r.SetAgentPermission("nobody", permReadOnly); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("存在しないエージェント: %v", err)
	}
	if err := r.SetAgentPermission("x", permWorkspace); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	st := r.statusLocked().Agents[0]
	last := r.messages[len(r.messages)-1]
	r.mu.Unlock()
	if st.Permission != permWorkspace || !st.PermissionOK || !strings.Contains(last.Text, "作業フォルダに書き込み可") {
		t.Fatalf("状態 %#v / 通知 %q", st, last.Text)
	}
	if got := permissionFrom(withPermission(context.Background(), a.permission)); got != permWorkspace {
		t.Fatalf("ctx の段階 %q", got)
	}

	// 再起動: 同じ設定フォルダから読み直すと戻る
	r2, a2 := newTestRoom(t)
	r2.mu.Lock()
	a2.Adapter = &claudeAdapter{bin: "claude"}
	r2.mu.Unlock()
	r2.SetConfigDir(r.configDir)
	r2.logDir = r.logDir
	r2.LoadSettings(nil)
	if a2.permission != permWorkspace {
		t.Fatalf("再起動後の段階 %q", a2.permission)
	}
	// 既定に戻すと保存からも消える
	if err := r.SetAgentPermission("x", permDefault); err != nil {
		t.Fatal(err)
	}
	r3, a3 := newTestRoom(t)
	r3.mu.Lock()
	a3.Adapter = &claudeAdapter{bin: "claude"}
	r3.mu.Unlock()
	r3.SetConfigDir(r.configDir)
	r3.logDir = r.logDir
	r3.LoadSettings(nil)
	if a3.permission != permDefault {
		t.Fatalf("既定に戻したのに段階が残った: %q", a3.permission)
	}
}

// 案8: 段階を選んだために *_ARGS から外したオプションは、status に載せて画面に出す
func TestPermissionOverride(t *testing.T) {
	t.Setenv("CODEX_ARGS", "-c sandbox_mode=workspace-write")
	r, a := newTestRoom(t)
	r.mu.Lock()
	a.Adapter = &codexAdapter{bin: "codex"}
	r.mu.Unlock()
	if err := r.SetAgentPermission("x", permReadOnly); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	st := r.statusLocked().Agents[0]
	r.mu.Unlock()
	if !slices.Equal(st.PermissionOverride, []string{"-c", "sandbox_mode=workspace-write"}) {
		t.Fatalf("外したオプション %q", st.PermissionOverride)
	}
}
