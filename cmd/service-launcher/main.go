// A static init shim inside the systemd filesystem jail. It never accepts
// caller-supplied executable paths or arguments; systemd owns the VMM process.
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

const socketPath = "/run/firecracker-builder/main.sock"

func cleanupStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket %s", path)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("another VMM is listening at %s", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("probe old socket: %w", err)
	}
	return os.Remove(path)
}

func main() {
	if err := cleanupStaleSocket(socketPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := syscall.Exec("/bin/firecracker", []string{"firecracker", "--api-sock", socketPath}, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
