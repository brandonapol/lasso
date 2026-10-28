package claude

import (
	"os/exec"
	"syscall"
)

// setDeathSignal makes the relay die with lasso, so it never outlives the
// daemon that owns it.
func setDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
