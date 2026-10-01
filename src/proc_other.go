//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func hideWindow(cmd *exec.Cmd) {}

// newProcessGroup は cmd を新しいプロセスグループで起動させる。killTree がグループごと終了させるため、
// CLI やスクリプトが起動した孫プロセスも残らない
func newProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree は cmd のプロセスグループ（newProcessGroup で作ったもの）をまとめて終了させる。
// グループを作っていない場合（失敗した場合）は cmd だけを終了させる
func killTree(cmd *exec.Cmd) {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
