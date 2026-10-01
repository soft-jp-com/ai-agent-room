//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// alive はプロセスがまだ動いているか（ゾンビは除く）を返す
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") // Linux ではゾンビ（Z）を除く。macOS では Kill の結果だけで判定
	return err != nil || !strings.Contains(string(b), ") Z ")
}

// waitPidFile は孫プロセスが書いた PID を読む
func waitPidFile(t *testing.T, path string) int {
	t.Helper()
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("孫プロセスの PID が書かれない")
	return 0
}

// 時間切れで終了させるとき、CLI が起動した孫プロセスも残さない
func TestRunProcessKillsGrandchildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx := withTurnTimeout(context.Background(), time.Second)
	start := time.Now()
	_, err := runProcess(ctx, "sh", []string{"-c", "sleep 60 & echo $! > " + pidFile + "; wait"}, "", t.TempDir())
	if err == nil {
		t.Fatal("時間切れにならない")
	}
	// 孫プロセスが出力をつかんだまま残ると、それが自然に終わるまで戻らない
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("時間切れのあと戻るまで %v かかった（孫プロセスを終了させていない）", d)
	}
	pid := waitPidFile(t, pidFile)
	time.Sleep(200 * time.Millisecond)
	if alive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("孫プロセス %d が残っている", pid)
	}
}

// コードブロックの実行を中止したときも、孫プロセスを残さない
func TestRunScriptKillsGrandchildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out strings.Builder
	start := time.Now()
	runScript(ctx, "bash", "sleep 60 &\necho $! > "+pidFile+"\nwait\n", t.TempDir(), &out)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("中止のあと戻るまで %v かかった（孫プロセスを終了させていない）", d)
	}
	pid := waitPidFile(t, pidFile)
	time.Sleep(200 * time.Millisecond)
	if alive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("孫プロセス %d が残っている: %s", pid, out.String())
	}
}
