//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || aix)

package process

import "os"

// On platforms without SIGTERM support the strongest available signal is
// used; the caller escalates to Kill shortly afterwards anyway.
func terminate(p *os.Process) error {
	return p.Kill()
}
