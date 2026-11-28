//go:build unix

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func spawnDetachedDaemon(homeDir string) error {
	if err := os.MkdirAll(homeDir, 0700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(homeDir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "-home", homeDir, "serve", "-web", "127.0.0.1:8765", "-web-fallback")
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}
