//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

// detach starts cmd without a console, in its own process group, so it
// outlives the terminal.
func detach(cmd *exec.Cmd) {
	const createNewProcessGroup, detachedProcess = 0x00000200, 0x00000008
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess, HideWindow: true}
}
