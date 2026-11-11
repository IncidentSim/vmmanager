package e2eci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// PrepareImage converts the verified upstream CI squashfs to a private ext4
// image without mounting it. Run under fakeroot: unsquashfs and mkfs.ext4 must
// share its ownership database, otherwise sshd rejects runner-owned /root.
// The sole guest change brings the older upstream unit to the product's exact
// supported fcnet format (settle udev before assigning MAC-derived addresses).
func PrepareImage(ctx context.Context, src, dst string, out io.Writer) error {
	if os.Getenv("FAKEROOTKEY") == "" {
		return errors.New("prepare-image requires fakeroot (no real root or mounts needed)")
	}
	if src == "" || dst == "" {
		return errors.New("--rootfs and --out are required")
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		return fmt.Errorf("output already exists or cannot be inspected: %s", dst)
	}
	work, err := os.MkdirTemp(filepath.Dir(dst), ".image-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tree := filepath.Join(work, "tree")
	run := func(name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}
	if err := run("unsquashfs", "-processors", "2", "-no-progress", "-d", tree, src); err != nil {
		return err
	}
	unit := filepath.Join(tree, "etc/systemd/system/fcnet.service")
	b, err := os.ReadFile(unit)
	if err != nil {
		return err
	}
	const old = "[Service]\nType=oneshot\nExecStart=/usr/local/bin/fcnet-setup.sh\n[Install]\nWantedBy=sshd.service"
	if strings.TrimRight(string(b), "\n") != old {
		return errors.New("unexpected upstream fcnet unit; refuse to patch")
	}
	b = []byte(strings.Replace(old, "ExecStart=", "ExecStartPre=/usr/bin/udevadm trigger --settle --subsystem-match=net --action=add\nExecStart=", 1) + "\n")
	if err := os.WriteFile(unit, b, 0644); err != nil {
		return err
	}
	image := filepath.Join(work, "rootfs.ext4")
	// 1 GiB fits the downloaded root tree; the backend grows private copies to
	// the selected 2 GiB profile. Disable orphan_file for older guest kernels.
	if err := run("mkfs.ext4", "-q", "-F", "-O", "^orphan_file", "-d", tree, image, "1024M"); err != nil {
		return err
	}
	// Prove compatibility using the actual offline SSH/network provisioners on
	// a disposable copy. Never publish credentials or fixture network in the base.
	probe := filepath.Join(work, "probe.ext4")
	if err := run("cp", "--reflink=auto", "--sparse=always", image, probe); err != nil {
		return err
	}
	master := filepath.Join(work, "master.key")
	if err := sshaccess.GenerateMasterKeyFile(master); err != nil {
		return err
	}
	store, err := sshaccess.OpenStore(filepath.Join(work, "credentials"), master)
	if err != nil {
		return err
	}
	access := &sshaccess.Provisioner{Store: store, User: "root"}
	if err := access.Install(ctx, "ci-probe", probe, ""); err != nil {
		return fmt.Errorf("image SSH provisioning: %w", err)
	}
	network := sshaccess.UbuntuFCNetProvisioner{}
	if err := network.InstallNetwork(ctx, probe, vmlib.GuestNetwork{Gateway: "10.213.0.1", Address: "10.213.0.2/30", Configuration: "mac-derived", DNS: []string{"1.1.1.1"}}); err != nil {
		return fmt.Errorf("image network provisioning: %w", err)
	}
	fmt.Fprintln(out, "verified actual offline SSH and guest-network provisioning on a disposable copy")
	// link is atomic and refuses concurrent replacement, unlike Rename.
	return os.Link(image, dst)
}
