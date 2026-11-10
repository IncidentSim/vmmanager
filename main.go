// A small Linux/KVM-only Firecracker image builder.
// It boots a COPY of an Ubuntu ext4 rootfs, installs PostgreSQL by SSH,
// gracefully shuts down the microVM and publishes the modified ext4 image.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	guestIP = "172.16.0.2"
	gateway = "172.16.0.1"
	macAddr = "06:00:AC:10:00:02" // Firecracker CI Ubuntu rootfs derives its IP from this MAC.
)

func main() {
	base := flag.String("base", "assets/ubuntu-base.ext4", "Ubuntu base ext4 rootfs")
	key := flag.String("ssh-key", "assets/id_ed25519", "SSH private key matching rootfs authorized_keys")
	output := flag.String("output", "images/ubuntu-postgres.ext4", "output Firecracker ext4 image")

	runImage := flag.String("run-image", "", "boot a writable disk inside /var/lib/firecracker-builder/work; supply a disposable copy")

	socket := flag.String("socket", serviceSocket, "existing Firecracker API Unix socket (VMM must already be running)")
	serve := flag.Bool("serve", false, "serve the fixed build operation over mutual TLS")
	listen := flag.String("listen", "0.0.0.0:19443", "mTLS listener address")
	tlsCA := flag.String("tls-ca", "", "trusted CA PEM file")
	tlsCert := flag.String("tls-cert", "", "server certificate PEM file")
	tlsKey := flag.String("tls-key", "", "server private key PEM file")
	jobDir := flag.String("job-dir", "", "server-controlled artifact directory (required with -serve)")
	flag.Parse()

	var err error
	if *serve {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if *runImage != "" || flag.NArg() != 0 {
			err = errors.New("-serve cannot be combined with -run-image or positional arguments")
		} else {
			err = serveBuilds(ctx, *listen, *tlsCA, *tlsCert, *tlsKey, *jobDir, *base, *key, *socket)
		}
	} else if *runImage != "" {
		err = runServiceImage(*runImage, *socket)
	} else {
		err = build(*base, *key, *output, *socket)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func build(base, key, output, socket string) (result error) {
	vmCtx, cancelVM := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelVM()
	return buildInWorkspace(vmCtx, workspace, socket, base, key, output)
}

func buildInWorkspace(vmCtx context.Context, root, socket, base, key, output string) (result error) {
	if runtime.GOOS != "linux" {
		return errors.New("Firecracker requires a Linux host with KVM")
	}
	lock, err := lockWorkspace(root)
	if err != nil {
		return err
	}
	defer lock.Close()
	vm, err := connectVM(vmCtx, socket)
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
	partial, err := os.CreateTemp(root, ".postgres-build-*.ext4")
	if err != nil {
		return err
	}
	partialPath := partial.Name()
	_ = partial.Close()
	safeCleanup := true
	defer func() {
		if safeCleanup {
			os.Remove(partialPath)
		}
	}()

	fmt.Println("[1/5] Copying Ubuntu rootfs")
	if err := run("cp", "--reflink=auto", "--sparse=always", base, partialPath); err != nil {
		return err
	}

	runDir, err := os.MkdirTemp("", "fc-image-builder-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(runDir)
	knownHosts := filepath.Join(runDir, "known_hosts")

	fmt.Println("[2/5] Configuring connected Firecracker via SDK Unix API")
	defer func() {
		if vm != nil {
			if stopErr := vm.Stop(); stopErr != nil {
				safeCleanup = false
				result = errors.Join(result, fmt.Errorf("%w; disk retained at %s", stopErr, partialPath))
			}
		}
	}()
	if err := vm.configure(vmCtx, root, partialPath); err != nil {
		return err
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- vm.Wait(vmCtx) }()

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
		probeCtx, cancel := context.WithTimeout(vmCtx, 4*time.Second)
		probe := exec.CommandContext(probeCtx, "ssh", append(append([]string{}, sshArgs...), "true")...)
		sshReady = probe.Run() == nil
		cancel()
		if sshReady {
			break
		}
		select {
		case vmErr := <-waitCh:
			return fmt.Errorf("firecracker stopped before SSH became ready: %v; %s", vmErr, journalHint())
		case <-vmCtx.Done():
			return vmCtx.Err()
		case <-readyBy.C:
			return fmt.Errorf("SSH timeout; check guest network and rootfs config; %s", journalHint())
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
# These commands run inside the guest over SSH, never on the host.
systemctl enable postgresql
systemctl start postgresql
runuser -u postgres -- psql -Atqc 'SHOW server_version;'
apt-get clean
rm -rf /var/lib/apt/lists/*
sync
`
	provisionCtx, cancel := context.WithTimeout(vmCtx, 10*time.Minute)
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
	shutdownCtx, cancelShutdown := context.WithTimeout(vmCtx, 10*time.Second)
	defer cancelShutdown()
	shutdown := exec.CommandContext(shutdownCtx, "ssh", append(append([]string{}, sshArgs...),
		`nohup sh -c 'sleep 2; sync; reboot' >/dev/null 2>&1 </dev/null &`)...)
	if out, err := shutdown.CombinedOutput(); err != nil {
		return fmt.Errorf("could not request guest shutdown: %w: %s", err, out)
	}

	select {
	case vmErr := <-waitCh:
		if vmErr != nil {
			return fmt.Errorf("firecracker stopped with error: %w; %s", vmErr, journalHint())
		}
	case <-vmCtx.Done():
		return vmCtx.Err()
	case <-time.After(60 * time.Second):
		return fmt.Errorf("VM did not exit after reboot; refusing to publish image; %s", journalHint())
	}

	// The guest has shut down, so its filesystem can safely become a new image.
	if err := vmCtx.Err(); err != nil {
		return err
	}
	if err := publishImage(partialPath, output); err != nil {
		return err
	}
	fmt.Printf("Built %s\n", output)
	fmt.Println("Boot a disposable copy with make run IMAGE=path/to/image.ext4; the service supplies the fixed guest kernel.")
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

func journalHint() string { return "VMM logs: journalctl -u firecracker-builder@main.service" }

// Stage on the destination filesystem, then publish with no-replace semantics.
func publishImage(source, output string) error {
	f, err := os.CreateTemp(filepath.Dir(output), ".publish-*.ext4")
	if err != nil {
		return err
	}
	temp := f.Name()
	f.Close()
	defer os.Remove(temp)
	if err := run("cp", "--reflink=auto", "--sparse=always", source, temp); err != nil {
		return err
	}
	f, err = os.OpenFile(temp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return unix.Renameat2(unix.AT_FDCWD, temp, unix.AT_FDCWD, output, unix.RENAME_NOREPLACE)
}
