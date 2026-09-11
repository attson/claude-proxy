//go:build !windows

package main

import (
	"os"
	"syscall"
)

// detachSysProcAttr 让后台代理进程脱离父进程会话(Setsid),常驻不随父进程退出。
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// forwardedSignals 是要转发给 claude 子进程的信号集(Unix 全套)。
func forwardedSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}
}
