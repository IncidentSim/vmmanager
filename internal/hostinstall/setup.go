package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/petruekhin/vibe-firecracker/internal/vmnet"
)

// SetupOptions drive the explicit host setup step.
type SetupOptions struct {
	Options
	User  string // unprivileged vmapi user that owns work/ and images/
	Apply bool   // false prints the plan only
	Out   io.Writer
	Run   vmnet.Runner
}

type dirSpec struct {
	path     string
	mode     fs.FileMode
	owner    bool // owned by the vmapi user:group instead of root
	storage  bool
	describe string
}

// Setup creates the EMPTY directories the units and vmapi expect: the VMM
// jail skeleton, /etc/firecracker-vms, and vms/{work,images} under an
// existing storage mount. It only creates what is missing, never changes
// existing ownership or modes, never writes pool.conf and never formats or
// mounts storage. Without Apply it prints what it would do.
func Setup(ctx context.Context, so SetupOptions) error {
	o := so.Options.withDefaults()
	if err := o.Validate(); err != nil {
		return err
	}
	if so.Out == nil {
		so.Out = io.Discard
	}
	if so.Apply && o.DestDir == "" && o.Euid() != 0 {
		return errors.New("host setup requires root")
	}
	var uid, gid = -1, -1
	if so.User != "" {
		u, err := user.Lookup(so.User)
		if err != nil {
			return fmt.Errorf("user %q: %w", so.User, err)
		}
		uid, _ = strconv.Atoi(u.Uid)
		gid, _ = strconv.Atoi(u.Gid)
		if uid == 0 {
			return errors.New("the vmapi user must not be root")
		}
	}
	jail := JailRoot
	specs := []dirSpec{{path: "/etc/firecracker-vms", mode: 0o755}, {path: "/run/firecracker-vms-net", mode: 0o755}, {path: jail, mode: 0o755}}
	for _, d := range []string{"bin", "kernel", "work", "run/slot", "dev/net", "proc", "sys", o.libexecdir()[1:]} {
		specs = append(specs, dirSpec{path: jail + "/" + d, mode: 0o755})
	}
	specs = append(specs,
		dirSpec{path: o.StorageRoot + "/vms", mode: 0o755, storage: true},
		dirSpec{path: o.StorageRoot + "/vms/work", mode: 0o770, owner: true, storage: true},
		dirSpec{path: o.StorageRoot + "/vms/images", mode: 0o770, owner: true, storage: true},
	)
	t, err := newTarget(o.DestDir)
	if err != nil {
		return err
	}
	for _, s := range specs {
		if s.owner && uid < 0 {
			return errors.New("--user is required to create the vmapi-owned work and images directories")
		}
		if s.storage && o.DestDir == "" {
			if fi, err := os.Stat(o.StorageRoot); err != nil || !fi.IsDir() {
				return fmt.Errorf("storage root %s does not exist: mount the storage volume first (never formatted by this tool)", o.StorageRoot)
			}
		}
	}
	for _, s := range specs {
		p, err := t.resolve(s.path)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(p); err == nil {
			fmt.Fprintf(so.Out, "exists   %s (left unchanged)\n", s.path)
			continue
		}
		if !so.Apply {
			fmt.Fprintf(so.Out, "would create %s %04o\n", s.path, uint32(s.mode))
			continue
		}
		if err := os.MkdirAll(p, s.mode); err != nil {
			return err
		}
		if err := os.Chmod(p, s.mode); err != nil {
			return err
		}
		if o.Euid() == 0 {
			ownerUID, ownerGID := 0, 0
			if s.owner {
				ownerUID, ownerGID = uid, gid
			}
			if err := os.Chown(p, ownerUID, ownerGID); err != nil {
				return err
			}
		}
		fmt.Fprintf(so.Out, "created  %s %04o\n", s.path, uint32(s.mode))
	}
	if o.DestDir != "" {
		return nil
	}
	if !so.Apply {
		fmt.Fprintln(so.Out, "would run: systemd-tmpfiles --create "+o.tmpfilesdir()+"/firecracker-vms.conf")
		return nil
	}
	r, err := so.Run.Run(ctx, "", "systemd-tmpfiles", "--create", o.tmpfilesdir()+"/firecracker-vms.conf")
	if err != nil || r.Code != 0 {
		return fmt.Errorf("systemd-tmpfiles --create failed: %v %s", err, r.Stderr)
	}
	fmt.Fprintln(so.Out, "created runtime tree via systemd-tmpfiles")
	return nil
}

// Enable activates exactly the privileged helper socket: daemon-reload, then
// enable and start fc-vm-helper.socket. It never enables VM or network
// units (the helper starts idle slots on demand) and never changes the firewall.
func Enable(ctx context.Context, o Options, apply bool, out io.Writer, run vmnet.Runner) error {
	o = o.withDefaults()
	if out == nil {
		out = io.Discard
	}
	steps := [][]string{{"daemon-reload"}, {"enable", "--now", "fc-vm-helper.socket"}}
	if !apply {
		for _, s := range steps {
			fmt.Fprintln(out, "would run: systemctl "+joinArgs(s))
		}
		return nil
	}
	if o.DestDir != "" {
		return errors.New("enable acts on the live host; DESTDIR is not supported")
	}
	if o.Euid() != 0 {
		return errors.New("enable requires root")
	}
	for _, s := range steps {
		r, err := run.Run(ctx, "", "systemctl", s...)
		if err != nil || r.Code != 0 {
			return fmt.Errorf("systemctl %s: %v %s", joinArgs(s), err, r.Stderr)
		}
		fmt.Fprintln(out, "ran: systemctl "+joinArgs(s))
	}
	return nil
}

func joinArgs(a []string) string { return strings.Join(a, " ") }
