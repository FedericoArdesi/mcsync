//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// detachFromConsoleSignals mette il processo figlio in un gruppo di processi
// nuovo: il Ctrl+C del terminale arriva solo all'agente, non a java.
func detachFromConsoleSignals(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
