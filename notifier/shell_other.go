//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package notifier

import "os/exec"

// CommandContext terminates the direct child; WaitDelay bounds pipe cleanup.
func configureShellCancellation(cmd *exec.Cmd) {}
