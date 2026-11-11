// vmhost installs, removes and checks the host side of the product.
//
//	vmhost manifest      print the files that would be installed (changes nothing)
//	vmhost install       place binaries, units, tmpfiles and examples (files only)
//	vmhost uninstall     remove exactly the files recorded by install
//	vmhost stage-check   root-free install/uninstall round trip into a temporary root
//	vmhost verify-units  render units and run systemd-analyze against them
//	vmhost preflight     read-only host readiness report
//	vmhost setup         create empty jail/work/images directories (needs --apply)
//	vmhost enable        enable the privileged helper socket (needs --apply)
//
// install never starts, enables or reloads anything and never touches
// configuration, keys, images, state, storage or firewall rules.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"syscall"

	"github.com/petruekhin/vibe-firecracker/internal/hostinstall"
	"github.com/petruekhin/vibe-firecracker/internal/vmnet"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "vmhost:", err)
		os.Exit(1)
	}
}

type common struct {
	hostinstall.Options
	pool string
}

func bind(fs *flag.FlagSet) *common {
	c := &common{}
	fs.StringVar(&c.Prefix, "prefix", "", "installation prefix (default /usr/local)")
	fs.StringVar(&c.DestDir, "destdir", "", "staging root; empty installs into the live filesystem")
	fs.StringVar(&c.SysconfDir, "sysconfdir", "", "system configuration directory (default /etc)")
	fs.StringVar(&c.ClientGroup, "group", "", "primary group of the unprivileged vmapi user (default: from SUDO_USER or the current user)")
	fs.StringVar(&c.AssetDir, "asset-dir", "", "directory holding the firecracker and vmlinux files")
	fs.StringVar(&c.StorageRoot, "storage-root", "", "existing storage mount holding vms/{work,images}")
	fs.StringVar(&c.BuildDir, "build-dir", "", "directory with the built executables")
	fs.StringVar(&c.Firecracker, "firecracker", "", "optional static Firecracker executable to install")
	fs.StringVar(&c.Kernel, "kernel", "", "optional kernel image to install")
	fs.BoolVar(&c.Force, "force", false, "replace unmanaged files / remove modified owned files")
	fs.StringVar(&c.pool, "pool-conf", hostinstall.DefaultPoolConf, "pool.conf location checked by preflight")
	return c
}

func defaultGroup() string {
	name := os.Getenv("SUDO_USER")
	var u *user.User
	var err error
	if name != "" {
		u, err = user.Lookup(name)
	} else {
		u, err = user.Current()
	}
	if err != nil {
		return ""
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		return ""
	}
	return g.Name
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: vmhost manifest|install|uninstall|stage-check|verify-units|preflight|setup|enable [flags]")
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := bind(fs)
	apply := fs.Bool("apply", false, "setup/enable: actually perform the steps (default prints the plan)")
	vmUser := fs.String("user", "", "setup: unprivileged vmapi user that owns work/ and images/")
	requireAnalyze := fs.Bool("require-systemd-analyze", false, "verify-units: fail instead of skipping when systemd-analyze is absent")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if c.ClientGroup == "" {
		c.ClientGroup = defaultGroup()
	}
	if c.BuildDir == "" {
		// A release archive is a flat directory of executables including vmhost
		// itself, so ./vmhost install works without a source tree.
		if exe, err := os.Executable(); err == nil {
			c.BuildDir = filepath.Dir(exe)
		}
	}
	switch cmd {
	case "manifest":
		plan, err := c.Plan()
		if err != nil {
			return err
		}
		m, err := hostinstall.BuildManifest(c.Options, plan)
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, hostinstall.FormatManifest(m))
	case "install":
		m, err := hostinstall.Install(c.Options)
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, hostinstall.FormatManifest(*m))
		fmt.Fprintf(stdout, "installed %d files. Nothing was started, enabled or reloaded.\nNext (explicit, as root): vmhost preflight; vmhost setup --apply --user USER; vmhost enable --apply\n", len(m.Files))
	case "uninstall":
		rep, err := hostinstall.Uninstall(c.Options)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %d files, %d directories; %d already absent\n", len(rep.Removed), len(rep.RemovedDirs), len(rep.Missing))
		for _, k := range rep.Kept {
			fmt.Fprintf(stdout, "kept (modified since install): %s\n", k)
		}
		fmt.Fprintln(stdout, "Configuration, keys, images, state and storage were not touched. Stop/disable units yourself first (systemctl disable --now fc-vm-helper.socket).")
		if len(rep.Kept) > 0 {
			return errors.New("some modified files were kept; rerun with --force to remove them")
		}
	case "stage-check":
		return stageCheck(c.Options, stdout)
	case "verify-units":
		plan, err := c.Plan()
		if err != nil {
			return err
		}
		if err := hostinstall.CheckUnits(c.Options, plan); err != nil {
			return err
		}
		ver, err := hostinstall.SystemdVerify(ctx, c.Options, plan)
		if errors.Is(err, hostinstall.ErrNoSystemdAnalyze) && !*requireAnalyze {
			fmt.Fprintln(stderr, "SKIPPED:", err)
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\nsystemd-analyze verify: OK (rendered units only; not installed)\n", ver)
	case "preflight":
		cs := hostinstall.Preflight(ctx, c.Options, c.pool, hostinstall.RealHost())
		fmt.Fprint(stdout, hostinstall.Format(cs))
		if hostinstall.Failed(cs) {
			return errors.New("preflight failed")
		}
	case "setup":
		return hostinstall.Setup(ctx, hostinstall.SetupOptions{Options: c.Options, User: *vmUser, Apply: *apply, Out: stdout, Run: vmnet.ExecRunner{}})
	case "enable":
		return hostinstall.Enable(ctx, c.Options, *apply, stdout, vmnet.ExecRunner{})
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}

// stageCheck installs into a throwaway DESTDIR, verifies exact modes and
// hashes, uninstalls, and requires the root to be empty again. It needs no
// privilege and touches nothing outside the temporary directory.
func stageCheck(o hostinstall.Options, out io.Writer) error {
	dir, err := os.MkdirTemp("", "vmhost-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	o.DestDir = dir
	m, err := hostinstall.Install(o)
	if err != nil {
		return fmt.Errorf("staged install: %w", err)
	}
	for _, f := range m.Files {
		fi, err := os.Stat(filepath.Join(dir, f.Path))
		if err != nil {
			return err
		}
		if got := fmt.Sprintf("%04o", uint32(fi.Mode().Perm())); got != f.Mode {
			return fmt.Errorf("%s: mode %s, manifest %s", f.Path, got, f.Mode)
		}
	}
	rep, err := hostinstall.Uninstall(o)
	if err != nil {
		return fmt.Errorf("staged uninstall: %w", err)
	}
	if len(rep.Removed) != len(m.Files) || len(rep.Kept) != 0 {
		return fmt.Errorf("uninstall removed %d of %d files", len(rep.Removed), len(m.Files))
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range left {
		// only directories created by the install may remain; none may hold files
		if err := filepath.WalkDir(filepath.Join(dir, e.Name()), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				return fmt.Errorf("left behind: %s", p)
			}
			return err
		}); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "stage-check ok: %d files installed with exact modes, then removed; no files left\n", len(m.Files))
	return nil
}
