//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// 案9（macOS・Linux）: 端末で開くスクリプトは、作業フォルダに移って CLI を起動し、終わったら終了コードを目印のファイルに書く。
// 引数は空白・引用符・$ を含んでもそのまま渡る
func TestConsoleScript(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "work dir")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	out, done := filepath.Join(dir, "out"), filepath.Join(dir, "done")
	// CLI の代わりに sh を使い、受け取った引数と作業フォルダを書き出して終了コード 3 で終わる
	script := consoleScript("sh", []string{"-c", `printf '%s|%s|%s' "$1" "$2" "$(pwd)" > "$3"; exit 3`, "x", "a b'c", `$HOME "q"`, out}, cwd, done)
	path := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("sh", path).Run(); err != nil {
		t.Fatalf("スクリプト: %v\n%s", err, script)
	}
	got, _ := os.ReadFile(out)
	if want := `a b'c|$HOME "q"|` + cwd; string(got) != want {
		t.Fatalf("引数・作業フォルダ %q, want %q", got, want)
	}
	s := newFileSession(dir, done)
	s.interval = 10 * time.Millisecond
	if code, err := s.Wait(); err != nil || code != 3 {
		t.Fatalf("終了コード %d / %v", code, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("待ち終わったのに一時フォルダが残った")
	}
}

// 目印が書かれなくても（窓を強制的に閉じた場合など）、［対話を終了］の Stop で待つのをやめる
func TestFileSessionStop(t *testing.T) {
	dir := t.TempDir()
	s := newFileSession(dir, filepath.Join(dir, "done"))
	s.interval = 10 * time.Millisecond
	go func() { time.Sleep(50 * time.Millisecond); s.Stop() }()
	if _, err := s.Wait(); !errors.Is(err, errConsoleStopped) {
		t.Fatalf("Stop で終わらない: %v", err)
	}
	if s.AutoDetect() {
		t.Fatal("閉じたことを自動で検知できる扱いになっている")
	}
}

// Linux で画面がない（DISPLAY も WAYLAND_DISPLAY もない）ときは窓を開かない
func TestTerminalCommandNoDisplay(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux のみ")
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	if _, err := terminalCommand("x"); !errors.Is(err, ErrInteractiveUnsupported) {
		t.Fatalf("画面がないのに窓を開こうとした: %v", err)
	}
}
