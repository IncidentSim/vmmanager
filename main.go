// A small Linux/KVM-only Firecracker image builder.
// It boots a COPY of an Ubuntu ext4 rootfs, installs PostgreSQL by SSH,
// gracefully shuts down the microVM and publishes the modified ext4 image.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	guestIP = "172.16.0.2"
	gateway = "172.16.0.1"
	macAddr = "06:00:AC:10:00:02" // Firecracker CI Ubuntu rootfs derives its IP from this MAC.
)

func main() {
	kernel := flag.String("kernel", "assets/vmlinux", "Firecracker-compatible guest kernel")
	base := flag.String("base", "assets/ubuntu-base.ext4", "Ubuntu base ext4 rootfs")
	key := flag.String("ssh-key", "assets/id_ed25519", "SSH private key matching rootfs authorized_keys")
	output := flag.String("output", "images/ubuntu-postgres.ext4", "output Firecracker ext4 image")
	tap := flag.String("tap", "tap-fcpg", "preconfigured Linux TAP device")
	binary := flag.String("firecracker", "assets/firecracker", "Firecracker executable")
	flag.Parse()

	if err := build(*kernel, *base, *key, *output, *tap, *binary); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func build(kernel, base, key, output, tap, binary string) error {
	if runtime.GOOS != "linux" {
		return errors.New("Firecracker requires a Linux host with KVM")
	}
	if os.Geteuid() != 0 {
		return errors.New("run as root (sudo); this demo uses a root-owned TAP and /dev/kvm")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("missing /dev/kvm: %w", err)
	}
	var err error
	kernel, err = absoluteFile(kernel)
	if err != nil {
		return err
	}
	base, err = absoluteFile(base)
	if err != nil {
		return err
	}
	key, err = absoluteFile(key)
	if err != nil {
		return err
	}
	binary, err = absoluteFile(binary)
	if err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	if _, err := os.Stat(output); err == nil {
		return fmt.Errorf("output already exists: %s", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}

	// Work with an independent filesystem image, preserving the original base.
	partial, err := os.CreateTemp(filepath.Dir(output), ".postgres-build-*.ext4")
	if err != nil {
		return err
	}
	partialPath := partial.Name()
	_ = partial.Close()
	defer os.Remove(partialPath) // no image is published unless all steps succeed

	fmt.Println("[1/5] Copying Ubuntu rootfs")
	if err := run("cp", "--reflink=auto", "--sparse=always", base, partialPath); err != nil {
		return err
	}

	runDir, err := os.MkdirTemp("", "fc-image-builder-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(runDir)
	confPath := filepath.Join(runDir, "firecracker.json")
	logPath := filepath.Join(runDir, "firecracker.log")
	knownHosts := filepath.Join(runDir, "known_hosts")

	config := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": kernel,
			"boot_args":         "console=ttyS0 reboot=k panic=1",
		},
		"drives": []map[string]any{{
			"drive_id": "rootfs", "path_on_host": partialPath,
			"is_root_device": true, "is_read_only": false,
		}},
		"machine-config": map[string]any{
			"vcpu_count": 2, "mem_size_mib": 1024,
		},
		"network-interfaces": []map[string]any{{
			"iface_id": "eth0", "guest_mac": macAddr, "host_dev_name": tap,
		}},
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(confPath, encoded, 0o600); err != nil {
		return err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	fmt.Println("[2/5] Starting Firecracker microVM")
	// --config-file starts the VM automatically; --no-api avoids a socket.
	vm := exec.Command(binary, "--no-api", "--config-file", confPath)
	vm.Stdout, vm.Stderr = logFile, logFile
	if err := vm.Start(); err != nil {
		return fmt.Errorf("start firecracker: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- vm.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			_ = vm.Process.Kill()
			<-waitCh
		}
	}()

	sshArgs := []string{
		"-T", "-i", key,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=2",
		"-o", "ConnectionAttempts=1",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "LogLevel=ERROR",
		"root@" + guestIP,
	}

	fmt.Println("[3/5] Waiting for SSH in Ubuntu")
	readyBy := time.NewTimer(90 * time.Second)
	defer readyBy.Stop()
	sshReady := false
	for !sshReady {
		probeCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		probe := exec.CommandContext(probeCtx, "ssh", append(append([]string{}, sshArgs...), "true")...)
		sshReady = probe.Run() == nil
		cancel()
		if sshReady {
			break
		}
		select {
		case vmErr := <-waitCh:
			stopped = true
			return fmt.Errorf("firecracker stopped before SSH became ready: %v; %s", vmErr, logTail(logPath))
		case <-readyBy.C:
			return fmt.Errorf("SSH timeout; check guest network and rootfs config; %s", logTail(logPath))
		case <-time.After(time.Second):
		}
	}

	fmt.Println("[4/5] Installing PostgreSQL inside Ubuntu")
	// This script runs IN THE GUEST, not in a host chroot or a container.
	provision := `set -euxo pipefail
ip route replace default via ` + gateway + ` dev eth0
printf 'nameserver 1.1.1.1\n' > /etc/resolv.conf
export DEBIAN_FRONTEND=noninteractive
apt-get -o Acquire::Retries=3 update
apt-get -o Acquire::Retries=3 install -y postgresql
systemctl enable postgresql
systemctl start postgresql
runuser -u postgres -- psql -Atqc 'SHOW server_version;'
apt-get clean
rm -rf /var/lib/apt/lists/*
sync
`
	provisionCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	install := exec.CommandContext(provisionCtx, "ssh", append(append([]string{}, sshArgs...), "bash -se")...)
	install.Stdin = strings.NewReader(provision)
	install.Stdout, install.Stderr = os.Stdout, os.Stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("guest provisioning failed: %w", err)
	}

	fmt.Println("[5/5] Rebooting guest to shut down Firecracker cleanly")
	// x86 Firecracker exits on guest reboot when boot args include reboot=k.
	// A delayed reboot allows SSH to return before its connection disappears.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	shutdown := exec.CommandContext(shutdownCtx, "ssh", append(append([]string{}, sshArgs...),
		`nohup sh -c 'sleep 2; sync; reboot' >/dev/null 2>&1 </dev/null &`)...)
	if out, err := shutdown.CombinedOutput(); err != nil {
		return fmt.Errorf("could not request guest shutdown: %w: %s", err, out)
	}

	select {
	case vmErr := <-waitCh:
		stopped = true
		if vmErr != nil {
			return fmt.Errorf("firecracker stopped with error: %w; %s", vmErr, logTail(logPath))
		}
	case <-time.After(60 * time.Second):
		return fmt.Errorf("VM did not exit after reboot; refusing to publish image; %s", logTail(logPath))
	}

	// The guest has shut down, so its filesystem can safely become a new image.
	if err := os.Rename(partialPath, output); err != nil {
		return err
	}
	fmt.Printf("Built %s\n", output)
	fmt.Println("Boot this ext4 file with the same guest kernel and a fresh TAP/VM config.")
	return nil
}

func absoluteFile(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("%s: %w", abs, err)
	}
	return abs, nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func logTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "no VM log available"
	}
	if len(data) > 4096 {
		data = data[len(data)-4096:]
	}
	return "VM log tail:\n" + string(data)
}
