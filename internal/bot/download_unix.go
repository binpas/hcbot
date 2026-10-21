//go:build unix

package bot

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel starts cmd in its own process group, and stops the full
// group when the context ends.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
