package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Unit is a parsed systemd unit: section -> ordered key/value pairs.
type Unit map[string][][2]string

// ParseUnit parses unit-file text (comments and blank lines skipped).
func ParseUnit(text string) Unit {
	u := Unit{}
	section := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[]")
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		u[section] = append(u[section], [2]string{k, v})
	}
	return u
}

// Values returns every value of key in section.
func (u Unit) Values(section, key string) []string {
	var out []string
	for _, kv := range u[section] {
		if kv[0] == key {
			out = append(out, kv[1])
		}
	}
	return out
}

// externalExecs are executables a unit may reference that this installer does
// not provide.
var externalExecs = map[string]bool{"/usr/bin/systemctl": true}

// CheckUnits validates rendered units against the planned executables: every
// Exec* program and every bind-mounted executable must be an installed
// product executable, so a unit can never reference a path that is not shipped.
func CheckUnits(o Options, plan []Entry) error {
	o = o.withDefaults()
	exes := map[string]bool{}
	for _, e := range plan {
		if e.Mode&0o111 != 0 {
			exes[e.Path] = true
		}
	}
	assets := map[string]bool{o.AssetDir + "/firecracker": true, o.AssetDir + "/vmlinux": true}
	var problems []string
	for _, e := range plan {
		if !strings.HasSuffix(e.Path, ".service") && !strings.HasSuffix(e.Path, ".socket") {
			continue
		}
		u := ParseUnit(string(e.Data))
		for _, key := range []string{"ExecStart", "ExecStartPre", "ExecStop", "ExecStopPost", "ExecReload"} {
			for _, v := range u.Values("Service", key) {
				prog := strings.Fields(strings.TrimLeft(v, "-+!@:"))
				if len(prog) == 0 || !strings.HasPrefix(prog[0], "/") {
					problems = append(problems, fmt.Sprintf("%s: %s=%q is not an absolute program", e.Path, key, v))
					continue
				}
				if !exes[prog[0]] && !externalExecs[prog[0]] && !strings.HasPrefix(prog[0], "%h/") {
					problems = append(problems, fmt.Sprintf("%s: %s program %s is not an installed executable", e.Path, key, prog[0]))
				}
			}
		}
		for _, key := range []string{"BindReadOnlyPaths", "BindPaths"} {
			for _, v := range u.Values("Service", key) {
				for _, spec := range strings.Fields(v) {
					src, _, _ := strings.Cut(spec, ":")
					src = strings.TrimPrefix(src, "-")
					if strings.HasPrefix(src, o.libexecdir()+"/") && !exes[src] {
						problems = append(problems, fmt.Sprintf("%s: %s=%s is not an installed executable", e.Path, key, src))
					}
					if strings.HasPrefix(src, o.AssetDir+"/") && !assets[src] {
						problems = append(problems, fmt.Sprintf("%s: %s=%s is not a known asset", e.Path, key, src))
					}
				}
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// ErrNoSystemdAnalyze means the real systemd verification could not run.
var ErrNoSystemdAnalyze = errors.New("systemd-analyze not found: unit files were NOT verified by systemd")

// SystemdVerify stages the plan into a throwaway root and runs the real
// `systemd-analyze verify` on the rendered system units. Nothing is installed
// on the host and no privilege is used. The returned error wraps
// ErrNoSystemdAnalyze when the tool is absent, so callers choose whether that
// is fatal (CI) or a loud skip (developer machines).
func SystemdVerify(ctx context.Context, o Options, plan []Entry) (string, error) {
	bin, err := exec.LookPath("systemd-analyze")
	if err != nil {
		return "", ErrNoSystemdAnalyze
	}
	o = o.withDefaults()
	root, err := os.MkdirTemp("", "vmhost-verify-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(root)
	unitdir := filepath.Join(root, o.unitdir())
	if err := os.MkdirAll(unitdir, 0o755); err != nil {
		return "", err
	}
	// Stub executables so ExecStart= paths resolve inside the throwaway root.
	for _, e := range plan {
		if e.Mode&0o111 == 0 || !strings.HasPrefix(e.Path, o.libexecdir()+"/") {
			continue
		}
		p := filepath.Join(root, e.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "etc/firecracker-vms", "dev"), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, "etc/firecracker-vms/pool.conf"), []byte("GUEST_PREFIX=10.213\nMAX_SLOTS=2\n"), 0o644); err != nil {
		return "", err
	}
	var units []string
	for _, e := range plan {
		if strings.HasPrefix(e.Path, o.unitdir()+"/") {
			if err := os.WriteFile(filepath.Join(unitdir, filepath.Base(e.Path)), e.Data, 0o644); err != nil {
				return "", err
			}
			base := filepath.Base(e.Path)
			switch base {
			case "firecracker-vm@.service":
				units = append(units, "firecracker-vm@0.service")
			case "firecracker-vm-network@.service":
				units = append(units, "firecracker-vm-network@0.service")
			case "fc-vm-helper@.service":
				units = append(units, "fc-vm-helper@1.service")
			default:
				units = append(units, base)
			}
		}
	}
	// Stub base targets and the budget slice: units carry default dependencies on
	// them and the throwaway root has no systemd of its own.
	for _, t := range []string{"sysinit", "basic", "shutdown", "sockets", "multi-user", "local-fs", "umount", "network"} {
		if err := os.WriteFile(filepath.Join(unitdir, t+".target"), []byte("[Unit]\nDescription=stub "+t+" target\n"), 0o644); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(unitdir, `system-firecracker\x2dbuilder.slice`), []byte("[Unit]\nDescription=stub slice\n"), 0o644); err != nil {
		return "", err
	}
	// Instance units are verified through their template copies.
	for _, u := range units {
		if i := strings.Index(u, "@"); i >= 0 && !strings.HasSuffix(u, "@.service") {
			tmpl := u[:i+1] + ".service"
			if _, err := os.Stat(filepath.Join(unitdir, tmpl)); err != nil {
				return "", err
			}
		}
	}
	args := append([]string{"verify", "--root=" + root, "--man=no"}, absUnits(unitdir, units)...)
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	ver, _ := exec.CommandContext(ctx, bin, "--version").Output()
	first, _, _ := strings.Cut(string(ver), "\n")
	if err != nil {
		return first, fmt.Errorf("systemd-analyze verify failed: %w\n%s", err, out)
	}
	return first, nil
}

func absUnits(dir string, units []string) []string {
	var out []string
	for _, u := range units {
		out = append(out, filepath.Join(dir, u))
	}
	return out
}
