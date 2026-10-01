package main

import (
	"errors"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// interactiveAdapter は対話モードに対応するテスト用のアダプタ
type interactiveAdapter struct{ fakeAdapter }

func (interactiveAdapter) InteractiveCommand(sessionID, model, _ string) (string, []string) {
	return "cli", []string{"--resume", sessionID, "--model", model}
}

// 案9: 各 CLI の対話モードの引数は、非対話用の引数を含まず、セッションの再開とモデルを引き継ぐ
func TestInteractiveCommand(t *testing.T) {
	t.Setenv("CLAUDE_ARGS", "--x")
	t.Setenv("CODEX_ARGS", "-c sandbox_mode=workspace-write")
	t.Setenv("AGY_ARGS", "")
	cases := []struct {
		name string
		ad   InteractiveStarter
		want []string
	}{
		{"claude", &claudeAdapter{bin: "claude"}, []string{"--model", "m1", "--resume", "s1", "--x"}},
		{"codex", &codexAdapter{bin: "node", pre: []string{"codex.js"}}, []string{"codex.js", "resume", "s1", "-m", "m1", "-c", "sandbox_mode=workspace-write"}},
		{"agy", &agyAdapter{bin: "agy"}, []string{"--conversation", "s1", "--model", "m1"}},
	}
	for _, c := range cases {
		_, args := c.ad.InteractiveCommand("s1", "m1", permDefault)
		// Claude の禁止ルール（--settings=…）は保護の設定しだいなので、比べる前に外す
		args = slices.DeleteFunc(args, func(s string) bool { return len(s) > 11 && s[:11] == "--settings=" })
		if !slices.Equal(args, c.want) {
			t.Errorf("%s: %q, want %q", c.name, args, c.want)
		}
		for _, bad := range []string{"-p", "--print", "exec", "--json", "--output-format"} {
			if slices.Contains(args, bad) {
				t.Errorf("%s: 非対話用の引数 %s が入っている", c.name, bad)
			}
		}
	}
	// セッションがなければ再開の引数を付けない（新しいセッションで起動する）
	if _, args := (&claudeAdapter{bin: "claude"}).InteractiveCommand("", "", permDefault); slices.Contains(args, "--resume") {
		t.Errorf("セッションがないのに --resume を付けた: %q", args)
	}
}

// 案9: 窓を開いている間はエージェントを会話に参加させず、閉じたら戻す。実行中・二重起動・非対応の環境では断る
func TestStartInteractive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd で窓の代わりのプロセスを動かすため Windows のみ")
	}
	r, a := newTestRoom(t)
	oldStart, oldOK := startConsole, interactiveOK
	t.Cleanup(func() { startConsole, interactiveOK = oldStart, oldOK })
	var gotArgs []string
	var gotCwd string
	startConsole = func(bin string, args []string, cwd string) (consoleSession, error) {
		gotArgs, gotCwd = args, cwd
		cmd := exec.Command("cmd", "/c", "ping -n 2 127.0.0.1 >nul") // 窓の代わりに約1秒で終わるプロセス
		hideWindow(cmd)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return newProcessSession(cmd.Process), nil
	}
	interactiveOK = true

	// 対応していない CLI は断る
	if err := r.StartInteractive("x"); !errors.Is(err, ErrInteractiveUnsupported) {
		t.Fatalf("対応していない CLI: %v", err)
	}
	r.mu.Lock()
	a.Adapter = interactiveAdapter{}
	a.sessionID, a.modelSel = "s1", "m1"
	a.state = "thinking"
	r.mu.Unlock()
	if err := r.StartInteractive("x"); !errors.Is(err, ErrAgentThinking) {
		t.Fatalf("実行中に起動した: %v", err)
	}
	r.mu.Lock()
	a.state = "idle"
	r.mu.Unlock()
	interactiveOK = false
	if err := r.StartInteractive("x"); !errors.Is(err, ErrInteractiveUnsupported) {
		t.Fatalf("窓を出せない環境で起動した: %v", err)
	}
	interactiveOK = true

	if err := r.StartInteractive("x"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	active, st := a.active(), r.statusLocked().Agents[0]
	workdir := r.workdir
	r.mu.Unlock()
	if active || !st.Interactive || !st.InteractiveOK {
		t.Fatalf("窓を開いている間も参加できる: active=%v status=%#v", active, st)
	}
	if !slices.Equal(gotArgs, []string{"--resume", "s1", "--model", "m1"}) || gotCwd != workdir {
		t.Fatalf("起動の引数 %q / 作業フォルダ %q", gotArgs, gotCwd)
	}
	if err := r.StartInteractive("x"); !errors.Is(err, ErrInteractiveRunning) {
		t.Fatalf("二重に起動した: %v", err)
	}
	// 窓が閉じたことを検知できる環境（Windows）では［対話を終了］を受け付けない
	if err := r.EndInteractive("x"); !errors.Is(err, ErrInteractiveNoEnd) {
		t.Fatalf("自動で検知できるのに対話を終了にできた: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		r.mu.Lock()
		back := a.active()
		r.mu.Unlock()
		if back {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("窓が閉じても会話に戻らない")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if m := lastMessage(r); m.Kind != "system" || !strings.Contains(m.Text, "窓が閉じました") {
		t.Fatalf("閉じたときの通知 %#v", m)
	}
}

// manualSession は、閉じたことを検知できない窓（macOS・Linux）の代わり。Stop されるまで待つ
type manualSession struct {
	stop chan struct{}
	once sync.Once
}

func (s *manualSession) Pid() int         { return 0 }
func (s *manualSession) AutoDetect() bool { return false }
func (s *manualSession) Stop()            { s.once.Do(func() { close(s.stop) }) }
func (s *manualSession) Wait() (int, error) {
	<-s.stop
	return -1, errConsoleStopped
}

// 案9（macOS・Linux）: 窓が閉じたことを検知できない環境では、画面に［対話を終了］を出し、押すと会話に戻す
func TestEndInteractive(t *testing.T) {
	r, a := newTestRoom(t)
	oldStart, oldOK := startConsole, interactiveOK
	t.Cleanup(func() { startConsole, interactiveOK = oldStart, oldOK })
	startConsole = func(string, []string, string) (consoleSession, error) {
		return &manualSession{stop: make(chan struct{})}, nil
	}
	interactiveOK = true
	r.mu.Lock()
	a.Adapter = interactiveAdapter{}
	r.mu.Unlock()

	if err := r.EndInteractive("x"); !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("開いていないのに終了にできた: %v", err)
	}
	if err := r.StartInteractive("x"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	st := r.statusLocked().Agents[0]
	opened := r.messages[len(r.messages)-1].Text
	r.mu.Unlock()
	if !st.Interactive || !st.InteractiveManual || !strings.Contains(opened, "［対話を終了］") {
		t.Fatalf("状態 %#v / 通知 %q", st, opened)
	}
	if err := r.EndInteractive("x"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		back, manual := a.active(), r.statusLocked().Agents[0].InteractiveManual
		r.mu.Unlock()
		if back && !manual {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("［対話を終了］で会話に戻らない")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m := lastMessage(r); !strings.Contains(m.Text, "終了にしました") {
		t.Fatalf("終了の通知 %q", m.Text)
	}
}
