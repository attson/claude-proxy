//go:build windows

package main

import (
	"os"
	"syscall"
)

// detachSysProcAttr 在 Windows 上让后台代理进程独立于父控制台。
// CREATE_NEW_PROCESS_GROUP(0x00000200)使其不随父进程的 Ctrl-C 一起被中断。
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200}
}

// forwardedSignals:Windows 只有 SIGINT/SIGTERM 有意义。
func forwardedSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}

// pidAlive:Windows 上 os.FindProcess 仅在进程存在时成功。
func pidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = proc.Release()
	return true
}
