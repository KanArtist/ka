//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in its own process group with its own hidden console,
// so it outlives the terminal. Unlike DETACHED_PROCESS (no console at all),
// CREATE_NO_WINDOW gives it a console without a window that the console
// programs it runs (git) inherit, so they do not each flash a new window.
func detach(cmd *exec.Cmd) {
	const createNewProcessGroup, createNoWindow = 0x00000200, 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | createNoWindow, HideWindow: true}
}
