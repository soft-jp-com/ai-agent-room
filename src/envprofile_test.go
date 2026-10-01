package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// シェルが1つも見つからない環境でも、プロフィールを作って投稿できる
func TestEnvProfileWithoutShells(t *testing.T) {
	oldLook, oldRun := envLookPath, envRunVersion
	t.Cleanup(func() { envLookPath, envRunVersion = oldLook, oldRun })
	envLookPath = func(string) (string, error) { return "", errors.New("not found") }
	envRunVersion = func(string, []string) (string, error) {
		t.Fatal("見つからないシェルを実行した")
		return "", nil
	}

	env := detectEnvironment()
	if !strings.Contains(env, "見つかりません") {
		t.Fatalf("見つからないことが書かれていない:\n%s", env)
	}
	r, _ := newTestRoom(t)
	r.SetEnvProfile(env, t.TempDir(), t.TempDir()) // rules.md がない
	m := lastMessage(r)
	if m.Kind != "system" || !strings.Contains(m.Text, "見つかりません") || !strings.Contains(m.Text, r.workdir) || !strings.Contains(m.Text, "■ 環境") || !strings.Contains(m.Text, "rules.md など）は編集しない") || strings.Contains(m.Text, "{{") {
		t.Fatalf("プロフィールの発言 %#v", m)
	}
}

// バージョンは最初の空でない行だけを使う。取得に失敗しても「あり」と書く
func TestProbeShell(t *testing.T) {
	oldLook, oldRun := envLookPath, envRunVersion
	t.Cleanup(func() { envLookPath, envRunVersion = oldLook, oldRun })
	envLookPath = func(s string) (string, error) { return s, nil }

	envRunVersion = func(string, []string) (string, error) { return "\r\nMicrosoft Windows [Version 10.0.26200]\r\n", nil }
	if got := probeShell(shellProbe{"cmd", []string{"cmd"}, nil}); got != "Microsoft Windows [Version 10.0.26200]" {
		t.Errorf("got %q", got)
	}
	envRunVersion = func(string, []string) (string, error) { return "", errors.New("timeout") }
	if got := probeShell(shellProbe{"pwsh", []string{"pwsh"}, nil}); !strings.HasPrefix(got, "あり") {
		t.Errorf("got %q", got)
	}
}

// 「新しい会話」でも先頭に載る。ルールは rules.md で差し替えられる。環境を調べる前は載せない
func TestEnvProfileOnReset(t *testing.T) {
	r, _ := newTestRoom(t)
	r.Reset()
	r.mu.Lock()
	n := len(r.messages)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("環境を調べる前に投稿した: %d 件", n)
	}
	cfg := t.TempDir()
	rules := filepath.Join(cfg, rulesFileName)
	if err := os.WriteFile(rules, []byte("- 独自のルール\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.SetEnvProfile("- OS: test", cfg, "")
	r.Reset()
	r.mu.Lock()
	msgs := append([]Message{}, r.messages...)
	r.mu.Unlock()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "- OS: test") || !strings.Contains(msgs[0].Text, "独自のルール") {
		t.Fatalf("新しい会話の先頭 %#v", msgs)
	}
}

// エージェントごとのできることは capabilities.json の宣言を載せ、宣言がなければ「不明」と出す
func TestEnvProfileCapabilities(t *testing.T) {
	r, _ := newTestRoom(t)
	dir := t.TempDir()

	r.SetEnvProfile("- OS: test", dir, "") // capabilities.json がない
	if m := lastMessage(r); !strings.Contains(m.Text, "■ エージェント") || !strings.Contains(m.Text, "- @x（X）: 書き込み 不明 / コマンド実行 不明 / 本番操作 不明") {
		t.Fatalf("宣言がないときの表示:\n%s", m.Text)
	}
	if err := os.WriteFile(filepath.Join(dir, capabilitiesFileName), []byte(`{"x": {"write": true, "prod": false}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Reset()
	if m := lastMessage(r); !strings.Contains(m.Text, "- @x（X）: 書き込み 可 / コマンド実行 不明 / 本番操作 不可") {
		t.Fatalf("宣言したときの表示:\n%s", m.Text)
	}
	// 壊れた JSON でも投稿はする（全員「不明」）
	if err := os.WriteFile(filepath.Join(dir, capabilitiesFileName), []byte(`{broken`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Reset()
	if m := lastMessage(r); !strings.Contains(m.Text, "書き込み 不明") {
		t.Fatalf("壊れた宣言のときの表示:\n%s", m.Text)
	}
}

// 設定は config フォルダを優先し、なければ旧い置き場所（実行ファイルのフォルダ）から読む
func TestEnvProfileConfigDirFallback(t *testing.T) {
	r, _ := newTestRoom(t)
	cfg, legacy := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(legacy, rulesFileName), []byte("- 旧い置き場所のルール"), 0o644)
	r.SetEnvProfile("- OS: test", cfg, legacy)
	if m := lastMessage(r); !strings.Contains(m.Text, "旧い置き場所のルール") {
		t.Fatalf("旧い置き場所を読んでいない:\n%s", m.Text)
	}
	os.WriteFile(filepath.Join(cfg, rulesFileName), []byte("- config のルール"), 0o644)
	r.Reset()
	if m := lastMessage(r); !strings.Contains(m.Text, "config のルール") || strings.Contains(m.Text, "旧い置き場所のルール") {
		t.Fatalf("config フォルダを優先していない:\n%s", m.Text)
	}
}
