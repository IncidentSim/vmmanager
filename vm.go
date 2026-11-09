package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

// sdkConfig describes a single guest; TAP/NAT are managed separately on the host.
func sdkConfig(kernel, disk, tap, socket string) firecracker.Config {
	return firecracker.Config{
		SocketPath:        socket,
		KernelImagePath:   kernel,
		KernelArgs:        "console=ttyS0 reboot=k panic=1",
		Drives:            []models.Drive{{DriveID: firecracker.String("rootfs"), PathOnHost: firecracker.String(disk), IsRootDevice: firecracker.Bool(true), IsReadOnly: firecracker.Bool(false)}},
		MachineCfg:        models.MachineConfiguration{VcpuCount: firecracker.Int64(2), MemSizeMib: firecracker.Int64(1024), Smt: firecracker.Bool(false)},
		NetworkInterfaces: firecracker.NetworkInterfaces{{StaticConfiguration: &firecracker.StaticNetworkConfiguration{HostDevName: tap, MacAddress: macAddr}}},
	}
}

func sdkCommand(ctx context.Context, binary, socket string, console io.Writer) *exec.Cmd {
	return firecracker.VMCommandBuilder{}.WithBin(binary).WithSocketPath(socket).WithStdout(console).WithStderr(console).Build(ctx)
}

func startSDKVM(ctx context.Context, kernel, disk, tap, binary, runDir string, console io.Writer) (*firecracker.Machine, error) {
	cfg := sdkConfig(kernel, disk, tap, filepath.Join(runDir, "firecracker.sock"))
	cmd := sdkCommand(ctx, binary, cfg.SocketPath, console)
	machine, err := firecracker.NewMachine(ctx, cfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		return nil, fmt.Errorf("create SDK machine: %w", err)
	}
	if err := machine.Start(ctx); err != nil {
		// Start can fail after the process was created but before InstanceStart.
		// Do not leave a VMM holding the writable disk in that case.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = machine.Wait(waitCtx)
		}
		return nil, fmt.Errorf("start SDK machine: %w", err)
	}
	return machine, nil
}
