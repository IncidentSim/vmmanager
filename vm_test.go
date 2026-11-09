package main

import (
	"context"
	"io"

	"reflect"
	"strings"
	"testing"
)

func TestSDKConfigPreservesGuestResources(t *testing.T) {
	cfg := sdkConfig("/kernel", "/disk.ext4", "tap-test", "/run/vm.sock")
	if cfg.SocketPath != "/run/vm.sock" || cfg.KernelImagePath != "/kernel" {
		t.Fatalf("wrong paths: %+v", cfg)
	}
	if *cfg.MachineCfg.VcpuCount != 2 || *cfg.MachineCfg.MemSizeMib != 1024 {
		t.Fatalf("wrong resources: %+v", cfg.MachineCfg)
	}
	if !strings.Contains(cfg.KernelArgs, "reboot=k") {
		t.Fatal("clean reboot exit must be preserved")
	}
	if len(cfg.Drives) != 1 || *cfg.Drives[0].PathOnHost != "/disk.ext4" || !*cfg.Drives[0].IsRootDevice || *cfg.Drives[0].IsReadOnly {
		t.Fatalf("wrong root drive: %+v", cfg.Drives)
	}
	if len(cfg.NetworkInterfaces) != 1 {
		t.Fatal("expected one interface")
	}
	net := cfg.NetworkInterfaces[0].StaticConfiguration
	if net == nil || net.HostDevName != "tap-test" || net.MacAddress != macAddr {
		t.Fatalf("wrong network: %+v", net)
	}
}

func TestSDKCommandUsesAPISocketInsteadOfConfigFile(t *testing.T) {
	cmd := sdkCommand(context.Background(), "/custom/firecracker", "/run/vm.sock", io.Discard)
	if cmd.Path != "/custom/firecracker" {
		t.Fatalf("binary: %s", cmd.Path)
	}
	if !reflect.DeepEqual(cmd.Args[1:], []string{"--api-sock", "/run/vm.sock"}) {
		t.Fatalf("unexpected SDK arguments: %q", cmd.Args)
	}
	if cmd.Stdout != io.Discard || cmd.Stderr != io.Discard {
		t.Fatal("guest console not redirected")
	}
}

func TestStartSDKVMRejectsMissingKernel(t *testing.T) {
	dir := t.TempDir()
	_, err := startSDKVM(context.Background(), "/missing/kernel", "/missing/disk", "tap-test", "/missing/firecracker", dir, io.Discard)
	if err == nil {
		t.Fatal("missing inputs must not start a VM")
	}
	if !strings.Contains(err.Error(), "kernel") {
		t.Fatalf("unhelpful error: %v", err)
	}

}
