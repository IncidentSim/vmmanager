// Per-slot init shim for firecracker-vm@N.service (proposed; source only). It
// runs inside the unit's filesystem jail, accepts no caller-supplied executable
// or arguments beyond the slot number systemd passes, and writes the immutable
// exit receipt that vmlib/local requires before publishing or deleting a disk.
//
//	vm-launcher run SLOT
//	vm-launcher record-exit SLOT   (ExecStopPost)
//
// The unit bind-mounts /run/firecracker-vms/slot-N at /run/slot, so the API
// socket and receipts of different slots never share a directory.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

const (
	maxSlots   = 64
	slotDir    = "/run/slot"
	socketPath = slotDir + "/api.sock"
)

var (
	invocationPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	slotPattern       = regexp.MustCompile(`^(0|[1-9][0-9]?)$`)
)

func parseSlot(s string) (int, error) {
	if !slotPattern.MatchString(s) {
		return 0, fmt.Errorf("invalid slot %q", s)
	}
	n, _ := strconv.Atoi(s)
	if n >= maxSlots {
		return 0, fmt.Errorf("slot %d out of range", n)
	}
	return n, nil
}

func launcherArgs(id string) ([]string, error) {
	if !invocationPattern.MatchString(id) {
		return nil, errors.New("invalid systemd INVOCATION_ID")
	}
	return []string{"firecracker", "--api-sock", socketPath, "--id", id}, nil
}

// recordExit publishes immutable evidence for the process that just exited.
func recordExit(dir, id, result, code, status string) error {
	if !invocationPattern.MatchString(id) {
		return errors.New("invalid systemd INVOCATION_ID")
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
	if err := f.Chmod(0o640); err != nil {
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
	return os.Link(f.Name(), filepath.Join(dir, id+".exit.json")) // fails if it exists
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

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func main() {
	if len(os.Args) != 3 {
		fail(errors.New("usage: vm-launcher run|record-exit SLOT"))
	}
	if _, err := parseSlot(os.Args[2]); err != nil {
		fail(err)
	}
	switch os.Args[1] {
	case "record-exit":
		if err := recordExit(slotDir, os.Getenv("INVOCATION_ID"), os.Getenv("SERVICE_RESULT"), os.Getenv("EXIT_CODE"), os.Getenv("EXIT_STATUS")); err != nil {
			fail(err)
		}
	case "run":
		args, err := launcherArgs(os.Getenv("INVOCATION_ID"))
		if err != nil {
			fail(err)
		}
		if err := cleanupStaleSocket(socketPath); err != nil {
			fail(err)
		}
		fail(syscall.Exec("/bin/firecracker", args, os.Environ()))
	default:
		fail(errors.New("unsupported command"))
	}
}
