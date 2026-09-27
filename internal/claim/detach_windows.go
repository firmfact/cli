//go:build windows

package claim

import "os/exec"

func detachFromTerminal(*exec.Cmd) {}
