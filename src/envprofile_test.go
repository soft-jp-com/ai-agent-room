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
	if text := env.text(langJa); !strings.Contains(text, "見つかりません") {
		t.Fatalf("見つからないことが書かれていない:\n%s", text)
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
	if got := probeShell(shellProbe{"cmd", []string{"cmd"}, nil}); got.Version != "Microsoft Windows [Version 10.0.26200]" {
		t.Errorf("got %+v", got)
	}
	envRunVersion = func(string, []string) (string, error) { return "", errors.New("timeout") }
	got := probeShell(shellProbe{"pwsh", []string{"pwsh"}, nil})
	e := &envInfo{OS: "test", Shells: []shellFound{got}}
	if ja, en := e.text(langJa), e.text(langEn); !strings.Contains(ja, "pwsh: あり") || !strings.Contains(en, "pwsh: available (version unknown)") {
		t.Errorf("got %+v\n%s\n%s", got, ja, en)
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
	r.SetEnvProfile(&envInfo{OS: "test"}, cfg, "")
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

	r.SetEnvProfile(&envInfo{OS: "test"}, dir, "") // capabilities.json がない
	if m := lastMessage(r); !strings.Contains(m.Text, "■ 参加者\n- @human（人間）:") || !strings.Contains(m.Text, "■ エージェント") || !strings.Contains(m.Text, "- @x（X）: 書き込み 不明 / コマンド実行 不明 / 本番操作 不明") {
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
	r.SetEnvProfile(&envInfo{OS: "test"}, cfg, legacy)
	if m := lastMessage(r); !strings.Contains(m.Text, "旧い置き場所のルール") {
		t.Fatalf("旧い置き場所を読んでいない:\n%s", m.Text)
	}
	os.WriteFile(filepath.Join(cfg, rulesFileName), []byte("- config のルール"), 0o644)
	r.Reset()
	if m := lastMessage(r); !strings.Contains(m.Text, "config のルール") || strings.Contains(m.Text, "旧い置き場所のルール") {
		t.Fatalf("config フォルダを優先していない:\n%s", m.Text)
	}
}

// 言語の設定が英語なら、環境とルールを英語で投稿する。rules.en.md を優先し、なければ rules.md を使う
func TestEnvProfileEnglish(t *testing.T) {
	r, _ := newTestRoom(t)
	if err := r.SetLang("fr"); err == nil {
		t.Fatal("対応していない言語を受け付けた")
	}
	if err := r.SetLang(langEn); err != nil {
		t.Fatal(err)
	}
	cfg := t.TempDir()
	r.SetEnvProfile(&envInfo{OS: "test", Shells: []shellFound{{Name: "cmd"}}}, cfg, "")
	m := lastMessage(r)
	for _, want := range []string{"■ Environment", "■ Participants\n- @human (the human):", "- OS: test", "  - cmd: not found", "- Working directory: " + r.workdir,
		"- @x (X): write unknown / run commands unknown / production unknown", "■ Rules for this chat", "rules.md and so on)"} {
		if !strings.Contains(m.Text, want) {
			t.Fatalf("%q がない:\n%s", want, m.Text)
		}
	}
	if strings.Contains(m.Text, "{{") || strings.Contains(m.Text, "ルール") {
		t.Fatalf("英語になっていない:\n%s", m.Text)
	}

	// 人間が書いた rules.md は英語の設定でも使う
	os.WriteFile(filepath.Join(cfg, rulesFileName), []byte("- 独自のルール"), 0o644)
	r.Reset()
	if m := lastMessage(r); !strings.Contains(m.Text, "独自のルール") {
		t.Fatalf("rules.md を使っていない:\n%s", m.Text)
	}
	os.WriteFile(filepath.Join(cfg, rulesFileNameEn), []byte("- my own rules"), 0o644)
	r.Reset()
	if m := lastMessage(r); !strings.Contains(m.Text, "my own rules") || strings.Contains(m.Text, "独自のルール") {
		t.Fatalf("rules.en.md を優先していない:\n%s", m.Text)
	}
	// 日本語に戻すと rules.en.md は使わない
	r.SetLang(langJa)
	r.Reset()
	if m := lastMessage(r); !strings.Contains(m.Text, "独自のルール") || !strings.Contains(m.Text, "■ 環境") {
		t.Fatalf("日本語に戻っていない:\n%s", m.Text)
	}
}
