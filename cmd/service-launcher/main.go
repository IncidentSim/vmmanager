// A static init shim inside the systemd filesystem jail. It never accepts
// caller-supplied executable paths or arguments; systemd owns the VMM process.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

const socketPath = "/run/firecracker-builder/main.sock"

var invocationPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func launcherArgs(id string) ([]string, error) {
	if !invocationPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid systemd INVOCATION_ID")
	}
	return []string{"firecracker", "--api-sock", socketPath, "--id", id}, nil
}

// ExecStopPost publishes immutable evidence for the process that just exited.
// Clients must not confuse a replacement process with proof of a clean exit.
func recordExit(dir, id, result, code, status string) error {
	if !invocationPattern.MatchString(id) {
		return fmt.Errorf("invalid systemd INVOCATION_ID")
	}
	data, err := json.Marshal(map[string]string{"invocation_id": id, "result": result, "exit_code": code, "exit_status": status})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".exit-receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0640); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Link(f.Name(), filepath.Join(dir, id+".exit.json"))
}

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
	if len(os.Args) == 2 && os.Args[1] == "record-exit" {
		if err := recordExit(filepath.Dir(socketPath), os.Getenv("INVOCATION_ID"), os.Getenv("SERVICE_RESULT"), os.Getenv("EXIT_CODE"), os.Getenv("EXIT_STATUS")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "unsupported arguments")
		os.Exit(1)
	}
	args, err := launcherArgs(os.Getenv("INVOCATION_ID"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := cleanupStaleSocket(socketPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := syscall.Exec("/bin/firecracker", args, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
