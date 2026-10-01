package main

// 対話モード（案9）の Windows 版: 新しいコンソール窓で CLI を起動し、プロセスの終了で窓が閉じたことを知る

import (
	"os"
	"syscall"
	"unsafe"
)

const createNewConsole = 0x00000010 // CREATE_NEW_CONSOLE

// startNewConsole は bin を新しいコンソール窓で起動する（対話モード。案9）。
// exec.Cmd は標準入出力を指定しないと NUL をつなぐため、CLI が端末ではないと判断してすぐ終わってしまう。
// そこで CreateProcess を直接呼び、標準入出力のハンドルを渡さずに新しいコンソールのものを使わせる
func startNewConsole(bin string, args []string, cwd string) (consoleSession, error) {
	cmdline := syscall.EscapeArg(bin)
	for _, a := range args {
		cmdline += " " + syscall.EscapeArg(a)
	}
	cl, err := syscall.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	var dir *uint16
	if cwd != "" {
		if dir, err = syscall.UTF16PtrFromString(cwd); err != nil {
			return nil, err
		}
	}
	si := &syscall.StartupInfo{}
	si.Cb = uint32(unsafe.Sizeof(*si))
	pi := &syscall.ProcessInformation{}
	if err := syscall.CreateProcess(nil, cl, nil, nil, false, createNewConsole|syscall.CREATE_UNICODE_ENVIRONMENT, nil, dir, si, pi); err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(pi.Process)
	syscall.CloseHandle(pi.Thread)
	p, err := os.FindProcess(int(pi.ProcessId)) // 終了を待つためのハンドルを開いてから、CreateProcess のハンドルを閉じる
	if err != nil {
		return nil, err
	}
	return newProcessSession(p), nil
}

var procProcessIdToSessionId = syscall.NewLazyDLL("kernel32.dll").NewProc("ProcessIdToSessionId")

// interactiveSupported は窓を表示できる環境か（Windows サービスが動くセッション 0 では窓を出せない）を返す
func interactiveSupported() bool {
	var session uint32
	ok, _, _ := procProcessIdToSessionId.Call(uintptr(syscall.Getpid()), uintptr(unsafe.Pointer(&session)))
	return ok != 0 && session != 0
}
