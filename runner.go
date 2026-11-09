package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// runSDKImage boots a writable disk; callers must supply a disposable copy.
func runSDKImage(kernel, disk, tap, binary, consolePath string) error {
	var err error
	for _, path := range []*string{&kernel, &disk, &binary} {
		*path, err = absoluteFile(*path)
		if err != nil {
			return err
		}
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("run as root; SDK runner requires KVM and an existing TAP")
	}
	dir, err := os.MkdirTemp("", "fc-sdk-run-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	var console io.Writer = os.Stdout
	if consolePath != "" {
		f, err := os.Create(consolePath)
		if err != nil {
			return err
		}
		defer f.Close()
		console = f
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	machine, err := startSDKVM(ctx, kernel, disk, tap, binary, dir, console)
	if err != nil {
		return err
	}
	fmt.Println("SDK VM running; guest reboot exits Firecracker")
	return machine.Wait(context.Background()) // wait for SDK to reap VMM even on cancellation
}
