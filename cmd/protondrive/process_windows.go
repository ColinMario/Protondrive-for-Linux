//go:build windows

package main

import (
	"os"
	"os/exec"
)

func configureDetachedProcess(cmd *exec.Cmd) {}

func processExists(pid int) bool {
	process, err := os.FindProcess(pid)
	return err == nil && process != nil
}

func interruptProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

func killProcess(pid int) error { return interruptProcess(pid) }
