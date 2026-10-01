//go:build !windows

package main

// 対話モード（案9）の macOS・Linux 版（試験的な対応）。
// 端末アプリ（macOS は Terminal.app、Linux は x-terminal-emulator などの端末エミュレータ）で、CLI を起動する sh のスクリプトを開く。
// 端末アプリは CLI の終了を待たずに戻るため、スクリプトが CLI の終了後に書く目印のファイル（終了コード）で閉じたことを知る。
// 窓を強制的に閉じた場合など目印が書かれないときのために、画面に［対話を終了］を出す（AutoDetect が false）。

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// shQuote は sh の引数として安全に渡せるよう、単一引用符で囲む
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// consoleScript は、作業フォルダに移って CLI を起動し、終わったら終了コードを done に書く sh のスクリプトを返す
func consoleScript(bin string, args []string, cwd, done string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if cwd != "" {
		fmt.Fprintf(&b, "cd %s || exit 1\n", shQuote(cwd))
	}
	b.WriteString(shQuote(bin))
	for _, a := range args {
		b.WriteString(" " + shQuote(a))
	}
	fmt.Fprintf(&b, "\necho $? > %s\n", shQuote(done))
	return b.String()
}

// linuxTerminals は Linux で探す端末エミュレータと、スクリプトを実行させる引数（見つかった順に使う）
var linuxTerminals = []struct {
	name string
	args func(script string) []string
}{
	{"x-terminal-emulator", func(s string) []string { return []string{"-e", "sh", s} }},
	{"gnome-terminal", func(s string) []string { return []string{"--", "sh", s} }},
	{"konsole", func(s string) []string { return []string{"-e", "sh", s} }},
	{"xfce4-terminal", func(s string) []string { return []string{"-x", "sh", s} }},
	{"xterm", func(s string) []string { return []string{"-e", "sh", s} }},
}

// terminalCommand はスクリプトを新しい端末の窓で開くコマンドを返す。窓を出せない環境ではエラー
func terminalCommand(script string) (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("osascript"); err != nil {
			return nil, ErrInteractiveUnsupported
		}
		// AppleScript の文字列の中なので、\ と " を逃がす
		sh := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace("sh " + shQuote(script))
		return exec.Command("osascript", "-e", `tell application "Terminal"`, "-e", "activate", "-e", `do script "`+sh+`"`, "-e", "end tell"), nil
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return nil, ErrInteractiveUnsupported
		}
		for _, t := range linuxTerminals {
			if bin, err := exec.LookPath(t.name); err == nil {
				return exec.Command(bin, t.args(script)...), nil
			}
		}
	}
	return nil, ErrInteractiveUnsupported
}

// interactiveSupported は窓を開ける環境か（macOS は osascript、Linux は画面と端末エミュレータがある）を返す
func interactiveSupported() bool {
	_, err := terminalCommand("x")
	return err == nil
}

// startNewConsole は CLI を新しい端末の窓で起動する
func startNewConsole(bin string, args []string, cwd string) (consoleSession, error) {
	dir, err := os.MkdirTemp("", "ai-agent-room-interactive-*")
	if err != nil {
		return nil, err
	}
	script, done := filepath.Join(dir, "run.sh"), filepath.Join(dir, "done")
	if err := os.WriteFile(script, []byte(consoleScript(bin, args, cwd, done)), 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	cmd, err := terminalCommand(script)
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	go cmd.Wait() // 端末アプリ（osascript など）の終了を回収する。CLI の終了は目印のファイルで知る
	return newFileSession(dir, done), nil
}

// fileSession は目印のファイルが書かれるのを待つ窓
type fileSession struct {
	dir, done string
	interval  time.Duration
	stop      chan struct{}
	stopOnce  sync.Once
}

func newFileSession(dir, done string) *fileSession {
	return &fileSession{dir: dir, done: done, interval: time.Second, stop: make(chan struct{})}
}

func (s *fileSession) Pid() int         { return 0 }
func (s *fileSession) AutoDetect() bool { return false }
func (s *fileSession) Stop()            { s.stopOnce.Do(func() { close(s.stop) }) }
func (s *fileSession) Wait() (int, error) {
	defer os.RemoveAll(s.dir)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		if b, err := os.ReadFile(s.done); err == nil && len(b) > 0 {
			code, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				return -1, errors.New("終了コードを読めません")
			}
			return code, nil
		}
		select {
		case <-s.stop:
			return -1, errConsoleStopped
		case <-t.C:
		}
	}
}
