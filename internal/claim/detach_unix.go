//go:build !windows

package claim

import (
	"os/exec"
	"syscall"
)

// detachFromTerminal starts the probe in its own session, without a
// controlling terminal, so an interactive rc cannot prompt or grab the tty.
// The session is also a process group, so a probe that runs out of time is
// stopped together with anything its rc file started, which would otherwise
// live on and keep its output open.
func detachFromTerminal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
