package sshaccess

import (
	"context"
	"fmt"
	"strings"
)

// Refuse operational overrides instead of attempting to interpret systemd's
// reset/list/continuation/precedence rules. Unknown guest layouts fail closed.
// Comment-only drop-ins remain byte-for-byte untouched. The single operational
// exception is our exact generated ExecStartPost, needed for derivatives.
func validateFCNetOverrides(ctx context.Context, g debugfs, disk string, readRegular func(string) (string, error)) error {
	unsupported := func() error { return fmt.Errorf("%w: unsupported fcnet systemd override", ErrUnsupportedImage) }
	roots := []string{"/etc/systemd/system.control", "/run/systemd/system.control", "/run/systemd/transient", "/run/systemd/generator.early", "/etc/systemd/system", "/etc/systemd/system.attached", "/run/systemd/system", "/run/systemd/system.attached", "/run/systemd/generator", "/usr/local/lib/systemd/system", "/usr/lib/systemd/system", "/run/systemd/generator.late"}
	// Ubuntu's merged-/usr alias is supported; a different symlink is not.
	lib, err := g.stat(ctx, disk, "/lib")
	if err != nil {
		return err
	}
	if lib.exists && lib.typ == "symlink" {
		out, err := g.run(ctx, disk, false, "stat /lib")
		if err != nil {
			return err
		}
		if !strings.Contains(out, `Fast link dest: "usr/lib"`) && !strings.Contains(out, `Fast link dest: "/usr/lib"`) {
			return unsupported()
		}
	} else if lib.exists {
		roots = append(roots, "/lib/systemd/system")
	}
	for _, base := range roots {
		if err := g.realAncestors(ctx, disk, base+"/entry"); err != nil {
			return err
		}
		unit := base + "/fcnet.service"
		if base != "/etc/systemd/system" {
			ino, err := g.stat(ctx, disk, unit)
			if err != nil {
				return err
			}
			if ino.exists {
				return unsupported()
			}
		}
		for _, dirname := range []string{"fcnet.service.d", "service.d"} {
			dir := base + "/" + dirname
			ino, err := g.stat(ctx, disk, dir)
			if err != nil {
				return err
			}
			if !ino.exists {
				continue
			}
			if ino.typ != "directory" {
				return unsupported()
			}
			if err := g.realAncestors(ctx, disk, dir+"/entry"); err != nil {
				return err
			}
			out, err := g.run(ctx, disk, false, "ls -p "+dir)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				fields := strings.Split(line, "/")
				if len(fields) != 8 || fields[0] != "" {
					return unsupported()
				}
				name := fields[5]
				if name == "." || name == ".." {
					continue
				}
				// Even currently ignored files are refused: do not guess loader behavior.
				if name == "" || strings.ContainsAny(name, " \t\r\n") {
					return unsupported()
				}
				path := dir + "/" + name
				data, err := readRegular(path)
				if err != nil {
					return err
				}
				if path == networkDropIn {
					if data != networkMarker+"[Service]\nExecStartPost=/bin/sh "+networkScript+"\n" {
						return unsupported()
					}
					continue
				}
				for _, text := range strings.Split(data, "\n") {
					text = strings.TrimSpace(text)
					if text != "" && !strings.HasPrefix(text, "#") && !strings.HasPrefix(text, ";") {
						return unsupported()
					}
				}
			}
		}
	}
	return nil
}
