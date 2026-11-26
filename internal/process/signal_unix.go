//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || aix

package process

import (
	"os"
	"syscall"
)

func terminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
