//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package notifier

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Kill the process group so descendants cannot outlive a timed-out command.
func configureShellCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
