package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// runServiceImage owns the workspace lock until the unit is confirmed stopped.
// It never deletes the caller's disk; callers must preserve it on failure.
func runServiceImage(disk string) (result error) {
	lock, err := lockWorkspace(workspace)
	if err != nil {
		return err
	}
	defer lock.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	vm, err := startServiceVM(ctx, disk)
	defer func() {
		if vm != nil {
			result = errors.Join(result, vm.Stop())
		}
	}()
	if err != nil {
		return err
	}
	fmt.Println("systemd VM running; guest reboot exits Firecracker; " + journalHint())
	return vm.Wait(ctx)
}
