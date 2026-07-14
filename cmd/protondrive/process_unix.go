//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func interruptProcess(pid int) error {
	return syscall.Kill(-pid, syscall.SIGINT)
}

func killProcess(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
