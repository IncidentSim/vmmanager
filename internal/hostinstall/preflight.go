package hostinstall

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"

	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
	"github.com/petruekhin/vibe-firecracker/internal/vmnet"
)

// Status of one preflight check.
type Status string

// Check statuses.
const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "FAIL"
)

// Check is one actionable preflight finding.
type Check struct {
	Name   string
	Status Status
	Detail string
	Fix    string
}

// Host abstracts the host so preflight is testable without KVM or root.
type Host struct {
	Stat      func(string) (fs.FileInfo, error)
	ReadFile  func(string) ([]byte, error)
	LookPath  func(string) (string, error)
	LookGroup func(string) error
	Statfs    func(string) (freeBytes uint64, err error)
	Runner    vmnet.Runner
}

// RealHost is the live host.
func RealHost() Host {
	return Host{
		Stat:     os.Stat,
		ReadFile: os.ReadFile,
		LookPath: func(n string) (string, error) {
			for _, d := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
				if p := d + "/" + n; fileExec(p) {
					return p, nil
				}
			}
			return exec.LookPath(n)
		},
		LookGroup: func(g string) error { _, err := user.LookupGroup(g); return err },
		Statfs: func(p string) (uint64, error) {
			var s syscall.Statfs_t
			if err := syscall.Statfs(p, &s); err != nil {
				return 0, err
			}
			return s.Bavail * uint64(s.Bsize), nil
		},
		Runner: vmnet.ExecRunner{},
	}
}

func fileExec(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// Failed reports whether any check failed.
func Failed(cs []Check) bool {
	for _, c := range cs {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Format renders the checks, one finding per line with its remedy.
func Format(cs []Check) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "[%4s] %s: %s\n", c.Status, c.Name, c.Detail)
		if c.Status != OK && c.Fix != "" {
			fmt.Fprintf(&b, "       fix: %s\n", c.Fix)
		}
	}
	return b.String()
}

var requiredTools = map[string]string{
	"ip": "iproute2", "iptables-nft": "iptables", "ip6tables-nft": "iptables",
	"iptables-nft-restore": "iptables", "ip6tables-nft-restore": "iptables",
	"conntrack": "conntrack", "systemctl": "systemd", "systemd-tmpfiles": "systemd",
}

// Preflight inspects the host read-only. poolConf may be empty for the default.
func Preflight(ctx context.Context, o Options, poolConf string, h Host) []Check {
	o = o.withDefaults()
	if poolConf == "" {
		poolConf = DefaultPoolConf
	}
	var cs []Check
	add := func(name string, st Status, detail, fix string) { cs = append(cs, Check{name, st, detail, fix}) }

	if fi, err := h.Stat("/dev/kvm"); err != nil || fi.Mode()&fs.ModeCharDevice == 0 {
		add("kvm", Fail, "/dev/kvm is missing or not a character device",
			"enable hardware virtualization (BIOS/VM nested virtualization) and load kvm_intel or kvm_amd")
	} else {
		add("kvm", OK, "/dev/kvm present", "")
	}
	if fi, err := h.Stat("/dev/net/tun"); err != nil || fi.Mode()&fs.ModeCharDevice == 0 {
		add("tun", Fail, "/dev/net/tun is missing", "modprobe tun")
	} else {
		add("tun", OK, "/dev/net/tun present", "")
	}
	if fi, err := h.Stat("/run/systemd/system"); err != nil || !fi.IsDir() {
		add("systemd", Fail, "the host is not booted with systemd", "the units require systemd as PID 1")
	} else {
		add("systemd", OK, "systemd is PID 1", "")
	}
	if _, err := h.LookPath("systemd-analyze"); err != nil {
		add("systemd-analyze", Warn, "not found; rendered units cannot be verified on this host", "install systemd")
	}
	var missing []string
	for _, t := range sortedKeys(requiredTools) {
		if _, err := h.LookPath(t); err != nil {
			missing = append(missing, t+" ("+requiredTools[t]+")")
		}
	}
	if len(missing) > 0 {
		add("tools", Fail, "missing: "+strings.Join(missing, ", "), "install the listed packages (iptables must provide the nft variants)")
	} else {
		add("tools", OK, "network and systemd tools present", "")
	}
	if b, err := h.ReadFile("/proc/sys/net/ipv4/ip_forward"); err != nil || strings.TrimSpace(string(b)) != "1" {
		add("ip_forward", Fail, "IPv4 forwarding is disabled", "sysctl -w net.ipv4.ip_forward=1 and persist it in /etc/sysctl.d (never changed by the installer)")
	} else {
		add("ip_forward", OK, "IPv4 forwarding enabled", "")
	}
	if o.ClientGroup != "" {
		if err := h.LookGroup(o.ClientGroup); err != nil {
			add("group", Fail, fmt.Sprintf("group %q does not exist", o.ClientGroup), "create it or pass the primary group of the vmapi user")
		} else {
			add("group", OK, "group "+o.ClientGroup+" exists", "")
		}
	}
	cs = append(cs, poolChecks(ctx, poolConf, h)...)
	cs = append(cs, storageChecks(o, h)...)
	for _, a := range []struct{ name, file string }{{"firecracker", "firecracker"}, {"kernel", "vmlinux"}} {
		p := o.AssetDir + "/" + a.file
		provided := (a.file == "firecracker" && o.Firecracker != "") || (a.file == "vmlinux" && o.Kernel != "")
		switch _, err := h.Stat(p); {
		case err == nil:
			add(a.name, OK, p+" present", "")
		case provided:
			add(a.name, OK, p+" will be installed from the supplied file", "")
		default:
			add(a.name, Fail, p+" is missing", "install it with --firecracker/--kernel (verified, pinned artifacts) or place it there")
		}
	}
	return cs
}

func sortedKeys(m map[string]string) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	// insertion sort keeps the dependency tiny
	for i := 1; i < len(k); i++ {
		for j := i; j > 0 && k[j] < k[j-1]; j-- {
			k[j], k[j-1] = k[j-1], k[j]
		}
	}
	return k
}

func poolChecks(ctx context.Context, poolConf string, h Host) []Check {
	fix := "copy the pool.conf example from the installed examples, choose a GUEST_PREFIX that overlaps nothing on this host, and install it root:root 0644"
	data, err := h.ReadFile(poolConf)
	if err != nil {
		return []Check{{"pool", Fail, poolConf + " is not readable: " + err.Error(), fix}}
	}
	pool, err := vmhelper.ParsePool(data)
	if err != nil {
		return []Check{{"pool", Fail, poolConf + " is invalid: " + err.Error(), fix}}
	}
	cs := []Check{{"pool", OK, fmt.Sprintf("%s: prefix %s, %d slots", poolConf, pool.Prefix, pool.MaxSlots), ""}}
	if fi, err := h.Stat(poolConf); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && (st.Uid != 0 || fi.Mode().Perm()&0o022 != 0) {
			cs = append(cs, Check{"pool-owner", Fail, poolConf + " must be root-owned and not group/other writable", "chown root:root and chmod 0644"})
		}
	}
	var bad []string
	for n := 0; n < pool.MaxSlots; n++ {
		s, err := vmnet.New(pool, n, h.Runner, "")
		if err != nil {
			continue
		}
		if err := s.CheckCollisions(ctx); err != nil {
			bad = append(bad, fmt.Sprintf("slot %d: %v", n, err))
		}
	}
	if len(bad) > 0 {
		cs = append(cs, Check{"subnet", Fail, strings.Join(bad, "; "), "choose another GUEST_PREFIX in pool.conf (and the backend config)"})
	} else {
		cs = append(cs, Check{"subnet", OK, "no route or address overlaps any slot /30", ""})
	}
	return cs
}

func storageChecks(o Options, h Host) []Check {
	var cs []Check
	root := o.StorageRoot
	fi, err := h.Stat(root)
	if err != nil || !fi.IsDir() {
		return []Check{{"storage", Fail, root + " does not exist", "mount the dedicated storage volume at " + root + " (the installer never partitions or formats)"}}
	}
	parent, perr := h.Stat(root + "/..")
	st, ok1 := fi.Sys().(*syscall.Stat_t)
	pt, ok2 := func() (*syscall.Stat_t, bool) {
		if perr != nil {
			return nil, false
		}
		s, ok := parent.Sys().(*syscall.Stat_t)
		return s, ok
	}()
	if !ok1 || !ok2 || st.Dev == pt.Dev {
		cs = append(cs, Check{"storage", Fail, root + " is not a mount point", "mount the storage volume there; units refuse to run on the root filesystem"})
	} else {
		cs = append(cs, Check{"storage", OK, root + " is a mount point", ""})
	}
	if free, err := h.Statfs(root); err == nil && free < 5<<30 {
		cs = append(cs, Check{"storage-space", Warn, fmt.Sprintf("only %d MiB free", free>>20), "images and VM disks need several GiB"})
	}
	for _, d := range []string{"/vms/work", "/vms/images"} {
		if _, err := h.Stat(root + d); err != nil {
			cs = append(cs, Check{"storage-dirs", Warn, root + d + " does not exist", "run: vmhost setup --apply"})
		}
	}
	return cs
}
