package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
)

const workspace = "/var/lib/firecracker-builder/work"
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

func configureGuest(ctx context.Context, socket, disk string) error {
	client := sdkClient(socket)
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
	return nil
}

// managedVM owns only a connection, never the externally managed VMM process.
type managedVM struct {
	socket string
	id     string
	// Also true for an uncertain InstanceStart response: the guest may be running.
	started bool
	// Set while disk configuration may have reached an unknown process.
	identityUncertain bool
}

var invocationID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func sdkClient(socket string) *firecracker.Client {
	return firecracker.NewClient(socket, logrus.NewEntry(logrus.New()), false)
}

// connectVM must run under the workspace lock, before copying or changing disks.
func connectVM(ctx context.Context, socket string) (*managedVM, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	info, err := sdkClient(socket).GetInstanceInfo(probeCtx)
	if err != nil {
		return nil, fmt.Errorf("connect to VMM socket %s: %w; operator must start/restart the VMM externally", socket, err)
	}
	if info.Payload == nil || info.Payload.State == nil || *info.Payload.State != "Not started" {
		return nil, fmt.Errorf("VMM is busy or not in Not started state; operator must restart it externally")
	}
	if info.Payload.ID == nil || !invocationID.MatchString(*info.Payload.ID) {
		return nil, fmt.Errorf("invalid VMM invocation id (expected 32 lowercase hex characters)")
	}
	return &managedVM{socket: socket, id: *info.Payload.ID}, nil
}

func (vm *managedVM) configure(ctx context.Context, root, disk string) error {
	current, err := connectVM(ctx, vm.socket)
	if err != nil {
		return err
	}
	if current.id != vm.id {
		return fmt.Errorf("VMM identity changed before configuration; operator must restart externally")
	}
	jail, err := prepareDisk(root, disk)
	if err != nil {
		return err
	}
	configureCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	vm.identityUncertain = true
	if err := configureGuest(configureCtx, vm.socket, jail); err != nil {
		return fmt.Errorf("configure VMM: %w; disk must be retained; operator must restart the VMM externally", err)
	}
	current, err = connectVM(configureCtx, vm.socket)
	if err != nil {
		return err
	}
	if current.id != vm.id {
		return fmt.Errorf("VMM identity changed during configuration; retain disk and restart externally")
	}
	vm.identityUncertain = false
	vm.started = true
	_, err = sdkClient(vm.socket).CreateSyncAction(configureCtx, &models.InstanceActionInfo{ActionType: firecracker.String("InstanceStart")})
	if err != nil {
		return fmt.Errorf("InstanceStart response uncertain: %w", err)
	}
	return nil
}

func startManagedVM(ctx context.Context, root, socket, disk string) (*managedVM, error) {
	vm, err := connectVM(ctx, socket)
	if err != nil {
		return nil, err
	}
	// A partially configured VMM can hold the disk open. Return it on failure so
	// cleanup refuses to discard the disk without a per-process exit receipt.
	return vm, vm.configure(ctx, root, disk)
}

type exitReceipt struct {
	InvocationID string `json:"invocation_id"`
	Result       string `json:"result"`
	ExitCode     string `json:"exit_code"`
	ExitStatus   string `json:"exit_status"`
}

func (vm *managedVM) receipt() (bool, error) {
	if vm.identityUncertain {
		return false, fmt.Errorf("disk configuration may have reached an unknown VMM process; operator must restart externally")
	}
	// Validate before deriving any filesystem path, even for internal callers.
	if !invocationID.MatchString(vm.id) {
		return false, fmt.Errorf("invalid VMM invocation id")
	}
	path := filepath.Join(filepath.Dir(vm.socket), vm.id+".exit.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read exit receipt: %w", err)
	}
	var r exitReceipt
	if err := json.Unmarshal(data, &r); err != nil {
		return false, fmt.Errorf("invalid exit receipt: %w", err)
	}
	if r.InvocationID != vm.id {
		return false, fmt.Errorf("exit receipt invocation mismatch")
	}
	if r.Result != "success" || r.ExitCode != "exited" || r.ExitStatus != "0" {
		return true, fmt.Errorf("VMM %s did not exit cleanly: result=%q exit_code=%q exit_status=%q", vm.id, r.Result, r.ExitCode, r.ExitStatus)
	}
	return true, nil
}

func (vm *managedVM) Wait(ctx context.Context) error {
	for {
		done, err := vm.receipt()
		if done || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for VMM %s exit receipt: %w", vm.id, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop requests a guest reboot through the SDK; it does not stop a host service.
// Never infer safety from a replacement responding on the same socket.
func (vm *managedVM) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := vm.abort(ctx); err != nil {
		return fmt.Errorf("cannot confirm clean VMM exit: %w; retain working disk; operator must restart the VMM externally", err)
	}
	return nil
}
func (vm *managedVM) abort(ctx context.Context) error {
	if done, err := vm.receipt(); done || err != nil {
		return err
	}
	if !vm.started {
		return fmt.Errorf("configuration did not reach InstanceStart; no safe API process cleanup is available")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	info, err := sdkClient(vm.socket).GetInstanceInfo(probeCtx)
	cancel()
	if err != nil {
		return err
	}
	if info.Payload == nil || info.Payload.ID == nil || *info.Payload.ID != vm.id {
		// A replacement may have appeared before ExecStopPost's receipt was visible.
		// Wait for the OLD receipt, never send an action to this process.
		if done, err := vm.receipt(); done || err != nil {
			return err
		}
		return fmt.Errorf("VMM identity changed; refusing to reboot replacement")
	}
	actionCtx, cancelAction := context.WithTimeout(ctx, 2*time.Second)
	_, err = sdkClient(vm.socket).CreateSyncAction(actionCtx, &models.InstanceActionInfo{ActionType: firecracker.String("SendCtrlAltDel")})
	cancelAction()
	// An HTTP failure does not prove the action failed: the VMM can exit
	// before sending its response. Only the receipt resolves that ambiguity.
	waitErr := vm.Wait(ctx)
	if waitErr != nil && err != nil {
		return errors.Join(fmt.Errorf("request guest reboot: %w", err), waitErr)
	}
	return waitErr
}
