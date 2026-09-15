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

// terminateProcess:Windows 无 SIGTERM 优雅投递,降级为 Kill(硬杀)。
// backend 被硬杀时其在飞请求会被切断——Windows 为次要平台,可接受;
// 主平台(Unix)走 SIGTERM 优雅排空。
func terminateProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}
