package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
)

const workspace = "/var/lib/firecracker-builder/work"
const serviceUnit = "firecracker-builder@main.service"
const serviceSocket = "/run/firecracker-builder/main.sock"

// jailDiskPath rejects lexical traversal and every symlink component, including
// the workspace itself. Only jail-visible paths may cross the privileged API.
func jailDiskPath(root, disk string) (string, error) {
	for _, part := range strings.Split(filepath.ToSlash(disk), "/") {
		if part == ".." {
			return "", fmt.Errorf("disk traversal is forbidden")
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	disk, err = filepath.Abs(disk)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, disk)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("disk must be inside %s", root)
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(disk, current), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink forbidden: %s", current)
		}
	}
	info, err := os.Stat(disk)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("disk is not a regular file")
	}
	return filepath.ToSlash(filepath.Join("/work", rel)), nil
}

func prepareDisk(root, disk string) (string, error) {
	jail, err := jailDiskPath(root, disk)
	if err != nil {
		return "", err
	}
	abs, _ := filepath.Abs(disk)
	base, _ := filepath.Abs(root)
	for dir := filepath.Dir(abs); dir != base; dir = filepath.Dir(dir) {
		if err := os.Chmod(dir, 0770); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(abs, 0660); err != nil {
		return "", err
	}
	return jail, nil
}

func lockWorkspace(root string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(root, ".client.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another image-builder client owns the workspace: %w", err)
	}
	return f, nil // close releases the lock; never unlink this stable inode
}

func configureVM(ctx context.Context, socket, disk string) error {
	client := firecracker.NewClient(socket, logrus.NewEntry(logrus.New()), false)
	cfg := models.MachineConfiguration{VcpuCount: firecracker.Int64(2), MemSizeMib: firecracker.Int64(1024), Smt: firecracker.Bool(false)}
	if _, err := client.PutMachineConfiguration(ctx, &cfg); err != nil {
		return err
	}
	boot := models.BootSource{KernelImagePath: firecracker.String("/kernel/vmlinux"), BootArgs: "console=ttyS0 reboot=k panic=1"}
	if _, err := client.PutGuestBootSource(ctx, &boot); err != nil {
		return err
	}
	drive := models.Drive{DriveID: firecracker.String("rootfs"), PathOnHost: firecracker.String(disk), IsRootDevice: firecracker.Bool(true), IsReadOnly: firecracker.Bool(false)}
	if _, err := client.PutGuestDriveByID(ctx, "rootfs", &drive); err != nil {
		return err
	}
	iface := models.NetworkInterface{IfaceID: firecracker.String("eth0"), HostDevName: firecracker.String("tap-fcpg"), GuestMac: macAddr}
	if _, err := client.PutGuestNetworkInterfaceByID(ctx, "eth0", &iface); err != nil {
		return err
	}
	_, err := client.CreateSyncAction(ctx, &models.InstanceActionInfo{ActionType: firecracker.String("InstanceStart")})
	return err
}

func systemctl(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--no-ask-password"}, args...)...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}
func serviceStatus(ctx context.Context) (string, error) {
	return systemctl(ctx, "show", "--property=ActiveState,SubState,Result,ExecMainStatus,ExecMainCode", serviceUnit)
}
func statusFields(raw string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			fields[k] = v
		}
	}
	return fields
}
func serviceOutcome(raw string) (bool, error) {
	s := statusFields(raw)
	if s["ActiveState"] == "active" && s["SubState"] == "exited" {
		// RemainAfterExit keeps exit metadata until the client has inspected it.
		if s["Result"] == "success" && s["ExecMainStatus"] == "0" && (s["ExecMainCode"] == "1" || s["ExecMainCode"] == "exited") {
			return true, nil
		}
		return true, fmt.Errorf("VMM did not exit cleanly: %s", strings.TrimSpace(raw))
	}
	switch s["ActiveState"] {
	case "active", "activating", "deactivating", "reloading":
		return false, nil
	case "inactive":
		// systemd encodes CLD_EXITED as 1; textual exited is accepted too.
		if s["Result"] == "success" && s["ExecMainStatus"] == "0" && (s["ExecMainCode"] == "1" || s["ExecMainCode"] == "exited") {
			return true, nil
		}
	}
	return true, fmt.Errorf("VMM did not exit cleanly: %s", strings.TrimSpace(raw))
}

type managedVM struct{}

func startServiceVM(ctx context.Context, disk string) (*managedVM, error) {
	return startManagedVM(ctx, workspace, serviceSocket, disk)
}

func startManagedVM(ctx context.Context, root, socket, disk string) (*managedVM, error) {
	jail, err := prepareDisk(root, disk)
	if err != nil {
		return nil, err
	}
	vm := &managedVM{}
	// Return vm even on failure: the caller must stop/verify before deleting disk.
	if _, err := systemctl(ctx, "start", serviceUnit); err != nil {
		return vm, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := firecracker.NewClient(socket, logrus.NewEntry(logrus.New()), false)
	for {
		info, err := client.GetInstanceInfo(readyCtx)
		if err == nil {
			if info.Payload.State == nil || *info.Payload.State != "Not started" {
				return vm, fmt.Errorf("unexpected VMM instance state: %v", info.Payload)
			}
			break
		}
		select {
		case <-readyCtx.Done():
			return vm, fmt.Errorf("waiting for service API: %w", readyCtx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := configureVM(readyCtx, socket, jail); err != nil {
		return vm, fmt.Errorf("configure VMM: %w", err)
	}
	return vm, nil
}
func (vm *managedVM) Wait(ctx context.Context) error {
	for {
		raw, err := serviceStatus(ctx)
		if err != nil {
			return err
		}
		done, err := serviceOutcome(raw)
		if done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func (vm *managedVM) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := systemctl(ctx, "stop", serviceUnit); err != nil {
		return err
	}
	raw, err := serviceStatus(ctx)
	if err != nil {
		return err
	}
	state := statusFields(raw)["ActiveState"]
	if state != "inactive" && state != "failed" {
		return fmt.Errorf("refusing disk cleanup: service state %q", state)
	}
	return nil
}
