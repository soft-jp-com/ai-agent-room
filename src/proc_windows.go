package main

import (
	"os/exec"
	"strconv"
	"syscall"
)

// hideWindow はサブプロセスのコンソールウィンドウを表示しない
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// newProcessGroup は Windows では何もしない（killTree は taskkill /T でプロセスツリーごと終了させる）
func newProcessGroup(*exec.Cmd) {}

// killTree は cmd と、その子孫のプロセスをまとめて終了させる
func killTree(cmd *exec.Cmd) {
	k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	hideWindow(k)
	_ = k.Run()
}
