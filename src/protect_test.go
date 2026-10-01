package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestProtectionDenyRules(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows のパスで確かめる")
	}
	p := Protection{SecretDir: `C:\Users\someone\AppData\Local\ai-agent-room`, EditOnly: []string{`C:\work\.claude`, `C:\work\ai-agent-room\start.bat`}}
	rules := p.DenyRules()
	for _, want := range []string{
		"Read(//c/Users/someone/AppData/Local/ai-agent-room/**)",
		"Edit(//c/Users/someone/AppData/Local/ai-agent-room/**)",
		"Bash(*Local/ai-agent-room*)",
		`Bash(*Local\ai-agent-room*)`,
		"PowerShell(*Local/ai-agent-room*)",
		"Edit(//c/work/.claude)",
		"Edit(//c/work/.claude/**)",
		"Edit(//c/work/ai-agent-room/start.bat)",
	} {
		if !slices.Contains(rules, want) {
			t.Errorf("%s がない: %v", want, rules)
		}
	}
}

// 保護するパスは作業ディレクトリと AI Agent Room のフォルダから決め、config/protected.json で足せる（環境に依存するパスを書かない）
func TestBuildProtection(t *testing.T) {
	secret, work, legacy := t.TempDir(), t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(secret, configDirName), 0o700)
	os.WriteFile(filepath.Join(secret, configDirName, protectedFileName), []byte(`{"paths": ["~/.aws", "  "]}`), 0o600)
	p := buildProtection(secret, work, legacy)
	home, _ := os.UserHomeDir()
	for _, want := range []string{
		filepath.Join(work, ".claude"), filepath.Join(work, "CLAUDE.md"), filepath.Join(work, "GEMINI.md"),
		filepath.Join(legacy, rulesFileName), filepath.Join(legacy, "start.bat"),
		filepath.Join(home, ".aws"),
	} {
		if !slices.Contains(p.EditOnly, want) {
			t.Errorf("%s が保護されていない: %v", want, p.EditOnly)
		}
	}
	if slices.Contains(p.EditOnly, "") {
		t.Error("空のパスを保護した")
	}
}

// コードブロックが保護するパスに触れるかを、区切り文字や大文字小文字の違いにかかわらず判定する
func TestTouchesProtected(t *testing.T) {
	old := protectMarkers
	t.Cleanup(func() { protectMarkers = old })
	setProtection(Protection{SecretDir: `C:\Users\a\AppData\Local\ai-agent-room`, EditOnly: []string{`C:\work\.claude`}})
	for code, want := range map[string]bool{
		`type C:\Users\a\AppData\Local\ai-agent-room\config\rules.md`: true,
		`notepad %LOCALAPPDATA%\ai-agent-room\config\rules.md`:        true,
		`cat ~/.claude/settings.json`:                                 true,
		`Set-Content .claude\settings.local.json '{}'`:                true,
		`claude --dangerously-skip-permissions`:                       true,
		`go build -o ai-agent-room.exe .`:                             false,
		`Get-Date`:                                                    false,
	} {
		if got := touchesProtected(code); got != want {
			t.Errorf("touchesProtected(%q) = %v, want %v", code, got, want)
		}
	}
	// 発言の保存時に blocks[].protected が付く
	r, _ := newTestRoom(t)
	m := postChat(r, "x", "```powershell\nGet-Content $env:LOCALAPPDATA\\ai-agent-room\\config\\rules.md\n```\n```powershell\nGet-Date\n```")
	if !m.Blocks[0].Protected || m.Blocks[1].Protected {
		t.Fatalf("blocks = %+v", m.Blocks)
	}
}

// 禁止ルールは --settings の JSON で渡す（パスに空白があっても崩れない）。ルールがなければ渡さず、enforced も false
func TestClaudeDenySettings(t *testing.T) {
	old, oldM := protection, protectMarkers
	t.Cleanup(func() { protection, protectMarkers = old, oldM })
	setProtection(Protection{})
	if s := claudeDenySettings(); s != "" {
		t.Fatalf("ルールがないのに %q", s)
	}
	if enforcesDeny(&Agent{Adapter: &claudeAdapter{}}) {
		t.Fatal("ルールがないのに enforced")
	}
	setProtection(Protection{EditOnly: []string{filepath.Join(t.TempDir(), "a b", "x")}})
	var v struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(claudeDenySettings()), &v); err != nil || !slices.Equal(v.Permissions.Deny, protection.DenyRules()) {
		t.Fatalf("settings = %s (%v)", claudeDenySettings(), err)
	}
	if !enforcesDeny(&Agent{Adapter: &claudeAdapter{}}) || enforcesDeny(&Agent{Adapter: fakeAdapter{}}) {
		t.Fatal("enforced の判定が違う")
	}
}

// AI Agent Room の設定は設定フォルダに保存し、以前のログディレクトリの settings.json は読み込んで移す
func TestSettingsMigrateToConfigDir(t *testing.T) {
	r, _ := newTestRoom(t)
	os.WriteFile(filepath.Join(r.logDir, "settings.json"), []byte(`{"max_hops": 7}`), 0o644)
	r.SetConfigDir(t.TempDir())
	r.LoadSettings(nil)
	if r.maxHops != 7 {
		t.Fatalf("以前の設定を読めていない: %d", r.maxHops)
	}
	if _, err := os.Stat(r.settingsPath()); err != nil {
		t.Fatalf("設定フォルダに移していない: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.logDir, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("ログディレクトリの settings.json が残っている: %v", err)
	}
}

// 保護の設定がなくても（起動時に失敗した場合など）、共通の目印は判定に使う
func TestTouchesProtectedWithoutSetup(t *testing.T) {
	old := protectMarkers
	t.Cleanup(func() { protectMarkers = old })
	protectMarkers = nil
	for _, code := range []string{`type %LOCALAPPDATA%\ai-agent-room\auth-token-x`, `echo {} > .claude\settings.json`, `claude --dangerously-skip-permissions`} {
		if !touchesProtected(code) {
			t.Errorf("%q を保護の対象と判定しない", code)
		}
	}
	if touchesProtected("Get-Date") {
		t.Error("関係のないコマンドを保護の対象と判定した")
	}
}
