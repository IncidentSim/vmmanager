package hostinstall

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
)

var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

// builtBinaries compiles the real product executables once (static, like make build).
func builtBinaries(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "hostinstall-bin-")
		if buildErr != nil {
			return
		}
		for _, n := range BuiltNames() {
			cmd := exec.Command("go", "build", "-o", filepath.Join(buildDir, n), "../../cmd/"+n)
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = errors.New(n + ": " + err.Error() + "\n" + string(out))
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func staged(t *testing.T) Options {
	t.Helper()
	return Options{DestDir: t.TempDir(), ClientGroup: "vmusers", BuildDir: builtBinaries(t), Euid: func() int { return 1000 }}
}

// tree returns "mode path" for every entry under root, for exact comparisons.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		kind := "f"
		if fi.IsDir() {
			kind = "d"
		} else if fi.Mode()&fs.ModeSymlink != 0 {
			kind = "l"
		}
		out = append(out, kind+" "+strconv.FormatUint(uint64(fi.Mode().Perm()), 8)+" /"+rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

const wantFiles = `0644 /etc/systemd/system/fc-vm-helper.socket
0644 /etc/systemd/system/fc-vm-helper@.service
0644 /etc/systemd/system/firecracker-vm-network@.service
0644 /etc/systemd/system/firecracker-vm@.service
0644 /etc/tmpfiles.d/firecracker-vms.conf
0755 /usr/local/bin/vmapi
0755 /usr/local/bin/vmbot
0755 /usr/local/bin/vmctl
0755 /usr/local/libexec/fc-vm-helper
0755 /usr/local/libexec/fc-vm-helper-client
0755 /usr/local/libexec/fc-vm-launcher
0755 /usr/local/libexec/fc-vm-network
0644 /usr/local/lib/systemd/user/vmapi.service
0644 /usr/local/lib/systemd/user/vmbot.service
0644 /usr/local/share/doc/vm-firecracker/examples/four-slot/backend.local.example.json
0644 /usr/local/share/doc/vm-firecracker/examples/four-slot/firecracker-vms.slice
0644 /usr/local/share/doc/vm-firecracker/examples/four-slot/pool.conf.example
0644 /usr/local/share/doc/vm-firecracker/examples/four-slot/resources.conf
0644 /usr/local/share/doc/vm-firecracker/examples/pool.conf.example
0644 /usr/local/share/doc/vm-firecracker/examples/vmapi/backend.local.example.json
0644 /usr/local/share/doc/vm-firecracker/examples/vmapi/idle-expiry.delta.example.json
0644 /usr/local/share/doc/vm-firecracker/examples/vmapi/nexterm.example.json
0644 /usr/local/share/doc/vm-firecracker/examples/vmapi/vmapi.example.json
0644 /usr/local/share/doc/vm-firecracker/examples/vmapi/vmapi.jwt.example.json
0644 /usr/local/share/doc/vm-firecracker/examples/vmbot/vmbot.example.json
0644 /usr/local/share/doc/vm-firecracker/examples/vmbot/vmbot.jwt.example.json
`

func TestManifestIsExact(t *testing.T) {
	o := staged(t)
	plan, err := o.Plan()
	if err != nil {
		t.Fatal(err)
	}
	m, err := BuildManifest(o, plan)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range m.Files {
		got = append(got, f.Mode+" "+f.Path)
	}
	want := strings.Split(strings.TrimSpace(wantFiles), "\n")
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("manifest differs\n got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestStagedInstallWritesExactTreeAndRendersUnits(t *testing.T) {
	o := staged(t)
	m, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range tree(t, o.DestDir) {
		if strings.HasPrefix(e, "f ") {
			files = append(files, strings.TrimPrefix(e, "f 0"))
		}
	}
	// every manifest file exists with its exact mode; plus the manifest itself
	if len(files) != len(m.Files)+1 {
		t.Fatalf("%d files on disk, %d in manifest", len(files), len(m.Files))
	}
	for _, f := range m.Files {
		fi, err := os.Stat(filepath.Join(o.DestDir, f.Path))
		if err != nil || strconv.FormatUint(uint64(fi.Mode().Perm()), 8) != strings.TrimPrefix(f.Mode, "0") {
			t.Errorf("%s: %v mode %v want %s", f.Path, err, fi, f.Mode)
		}
	}
	helper, _ := os.ReadFile(filepath.Join(o.DestDir, "/etc/systemd/system/fc-vm-helper.socket"))
	if !strings.Contains(string(helper), "SocketGroup=vmusers") {
		t.Fatalf("group not rendered:\n%s", helper)
	}
	vm, _ := os.ReadFile(filepath.Join(o.DestDir, "/etc/systemd/system/firecracker-vm@.service"))
	for _, want := range []string{
		"ExecStart=/usr/local/libexec/fc-vm-launcher run %i", "Group=vmusers",
		"BindReadOnlyPaths=/opt/firecracker-builder/firecracker:/bin/firecracker",
		"RequiresMountsFor=/var/lib/firecracker-builder/storage",
	} {
		if !strings.Contains(string(vm), want) {
			t.Errorf("rendered VM unit missing %q", want)
		}
	}
	api, _ := os.ReadFile(filepath.Join(o.DestDir, "/usr/local/lib/systemd/user/vmapi.service"))
	if !strings.Contains(string(api), "ExecStart=/usr/local/bin/vmapi -config %h/.config/vmapi/vmapi.json") {
		t.Fatalf("vmapi unit:\n%s", api)
	}
	// no staging path, repo path or user home leaks into any installed file
	for _, f := range m.Files {
		b, _ := os.ReadFile(filepath.Join(o.DestDir, f.Path))
		if strings.HasPrefix(f.Path, "/usr/local/bin") || strings.HasPrefix(f.Path, "/usr/local/libexec") {
			continue
		}
		for _, bad := range []string{o.DestDir, o.BuildDir, "@CLIENT_GROUP@", "@LIBEXECDIR@", "@BINDIR@", "@ASSET_DIR@", "@STORAGE_ROOT@"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %q", f.Path, bad)
			}
		}
	}
}

func TestPrefixAndSysconfdirAreHonored(t *testing.T) {
	o := staged(t)
	o.Prefix, o.SysconfDir, o.AssetDir, o.StorageRoot = "/opt/vmx", "/srv/etc", "/srv/assets", "/mnt/vmstore"
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(o.DestDir, "/srv/etc/systemd/system/firecracker-vm@.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/opt/vmx/libexec/fc-vm-launcher", "/srv/assets/vmlinux:/kernel/vmlinux", "/mnt/vmstore/vms/work:/work"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %q in\n%s", want, b)
		}
	}
	for _, p := range []string{"/opt/vmx/bin/vmapi", "/opt/vmx/libexec/fc-vm-network", "/srv/etc/tmpfiles.d/firecracker-vms.conf", "/opt/vmx/share/vm-firecracker/install-manifest.json"} {
		if _, err := os.Stat(filepath.Join(o.DestDir, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestInstallUninstallRoundTripPreservesUserData(t *testing.T) {
	o := staged(t)
	keep := map[string]string{
		"/etc/firecracker-vms/pool.conf":                            "GUEST_PREFIX=10.99\nMAX_SLOTS=2\n",
		"/home/u/.config/vmapi/vmapi.json":                          `{"keep":true}`,
		"/var/lib/firecracker-builder/storage/vms/images/base.ext4": "image",
		"/var/lib/firecracker-vms/jail/work/disk":                   "state",
		"/usr/local/bin/unrelated":                                  "other tool",
	}
	for p, c := range keep {
		full := filepath.Join(o.DestDir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := tree(t, o.DestDir)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	// idempotent second install
	if _, err := Install(o); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	rep, err := Uninstall(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Kept) != 0 || len(rep.Removed) != 26 {
		t.Fatalf("report: %+v", rep)
	}
	if after := tree(t, o.DestDir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("tree changed by install/uninstall\nbefore:\n%s\nafter:\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	for p, c := range keep {
		b, err := os.ReadFile(filepath.Join(o.DestDir, p))
		if err != nil || string(b) != c {
			t.Errorf("%s was modified: %v %q", p, err, b)
		}
	}
}

func TestUninstallKeepsModifiedFilesUnlessForced(t *testing.T) {
	o := staged(t)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(o.DestDir, "/etc/systemd/system/fc-vm-helper.socket")
	if err := os.WriteFile(unit, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Uninstall(o)
	if err != nil || len(rep.Kept) != 1 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if _, err := os.Stat(unit); err != nil {
		t.Fatal("modified file removed without --force")
	}
	o.Force = true
	if rep, err = Uninstall(o); err != nil || len(rep.Kept) != 0 {
		t.Fatalf("forced: rep=%+v err=%v", rep, err)
	}
	if _, err := os.Stat(unit); err == nil {
		t.Fatal("forced uninstall left file")
	}
}

func TestUnmanagedFilesAreNotOverwrittenWithoutForce(t *testing.T) {
	o := staged(t)
	p := filepath.Join(o.DestDir, "/usr/local/libexec/fc-vm-helper")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("hand installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err == nil || !strings.Contains(err.Error(), "fc-vm-helper") {
		t.Fatalf("err = %v", err)
	}
	// nothing at all was written
	if _, err := os.Stat(filepath.Join(o.DestDir, "/usr/local/bin/vmapi")); err == nil {
		t.Fatal("partial install after refusal")
	}
	o.Force = true
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinksAndEscapesAreRefused(t *testing.T) {
	t.Run("symlinked ancestor", func(t *testing.T) {
		o := staged(t)
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(o.DestDir, "etc")); err != nil {
			t.Fatal(err)
		}
		if _, err := Install(o); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("err = %v", err)
		}
		if ents, _ := os.ReadDir(outside); len(ents) != 0 {
			t.Fatalf("wrote through symlink: %v", ents)
		}
	})
	t.Run("symlinked target", func(t *testing.T) {
		o := staged(t)
		victim := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(o.DestDir, "usr/local/bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, filepath.Join(bin, "vmapi")); err != nil {
			t.Fatal(err)
		}
		if _, err := Install(o); err == nil {
			t.Fatal("symlinked target accepted")
		}
		if b, _ := os.ReadFile(victim); string(b) != "precious" {
			t.Fatal("symlink target modified")
		}
	})
	t.Run("unsafe options", func(t *testing.T) {
		for _, mod := range []func(*Options){
			func(o *Options) { o.Prefix = "/usr/../etc" },
			func(o *Options) { o.Prefix = "relative" },
			func(o *Options) { o.Prefix = "/usr/local x" },
			func(o *Options) { o.SysconfDir = "/etc/%h" },
			func(o *Options) { o.AssetDir = "/opt/a\nExecStart=/bin/sh" },
			func(o *Options) { o.StorageRoot = "/" },
			func(o *Options) { o.ClientGroup = "x;id" },
			func(o *Options) { o.DestDir = "relative/stage" },
		} {
			o := staged(t)
			mod(&o)
			if _, err := Install(o); err == nil {
				t.Errorf("unsafe options accepted: %+v", o)
			}
		}
	})
	t.Run("manifest with escaping path", func(t *testing.T) {
		o := staged(t)
		if _, err := Install(o); err != nil {
			t.Fatal(err)
		}
		mp := filepath.Join(o.DestDir, "usr/local", ManifestRel)
		b, _ := os.ReadFile(mp)
		var m Manifest
		_ = json.Unmarshal(b, &m)
		m.Files = append(m.Files, ManifestFile{Path: "/../../etc/passwd", Mode: "0644", SHA256: "x"})
		b, _ = json.Marshal(m)
		_ = os.WriteFile(mp, b, 0o644)
		if _, err := Uninstall(o); err == nil {
			t.Fatal("tampered manifest accepted")
		}
	})
}

func TestLiveInstallRequiresRootAndTouchesNothing(t *testing.T) {
	o := Options{ClientGroup: "vmusers", BuildDir: builtBinaries(t), Euid: func() int { return 1000 }}
	for name, f := range map[string]func() error{
		"install":   func() error { _, err := Install(o); return err },
		"uninstall": func() error { _, err := Uninstall(o); return err },
	} {
		if err := f(); err == nil || !strings.Contains(err.Error(), "root") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestExecutablesMustExistAndJailBinariesBeStatic(t *testing.T) {
	o := staged(t)
	o.BuildDir = t.TempDir()
	if _, err := o.Plan(); err == nil || !strings.Contains(err.Error(), "make build") {
		t.Fatalf("err = %v", err)
	}
	// a dynamically linked ELF (the system shell) must not be accepted for the jail
	dyn, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	if err := checkExecutable(dyn, true); err == nil {
		t.Skip("sh is static on this system")
	} else if !strings.Contains(err.Error(), "static") {
		t.Fatal(err)
	}
}

func TestUnitsReferenceOnlyInstalledExecutables(t *testing.T) {
	o := staged(t)
	plan, err := o.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckUnits(o, plan); err != nil {
		t.Fatal(err)
	}
	var broken []Entry
	for _, e := range plan {
		if e.Path != "/usr/local/libexec/fc-vm-network" {
			broken = append(broken, e)
		}
	}
	err = CheckUnits(o, broken)
	if err == nil || !strings.Contains(err.Error(), "fc-vm-network") {
		t.Fatalf("missing network tool not detected: %v", err)
	}
}

func TestSystemdAnalyzeAcceptsRenderedUnits(t *testing.T) {
	o := staged(t)
	plan, err := o.Plan()
	if err != nil {
		t.Fatal(err)
	}
	ver, err := SystemdVerify(context.Background(), o, plan)
	if errors.Is(err, ErrNoSystemdAnalyze) {
		if os.Getenv("VMHOST_REQUIRE_SYSTEMD_ANALYZE") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ver)
}

// --- unit contract (ported from the former Python deployment tests) ----------

func renderedUnit(t *testing.T, name string) Unit {
	t.Helper()
	o := staged(t)
	plan, err := o.Plan()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range plan {
		if filepath.Base(e.Path) == name {
			return ParseUnit(string(e.Data))
		}
	}
	t.Fatalf("no unit %s", name)
	return nil
}

func eq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestVMUnitContract(t *testing.T) {
	u := renderedUnit(t, "firecracker-vm@.service")
	if len(u.Values("Service", "ExecStartPre")) != 0 {
		t.Error("ExecStartPre inside the jail")
	}
	for _, key := range []string{"ExecStart", "ExecStopPost", "ExecStop"} {
		for _, v := range u.Values("Service", key) {
			if strings.Contains(v, "fc-vm-network") || strings.HasPrefix(v, "+") || strings.HasPrefix(v, "!") || strings.HasPrefix(v, "@") || strings.HasPrefix(v, "-") {
				t.Errorf("%s=%s must not run networking or use privilege prefixes", key, v)
			}
		}
	}
	eq(t, u.Values("Service", "RootDirectory"), "/var/lib/firecracker-vms/jail")
	for _, want := range []string{"firecracker-vm-network@%i.service"} {
		if !contains(u.Values("Unit", "Requires"), want) || !contains(u.Values("Unit", "After"), want) {
			t.Errorf("missing Requires/After %s", want)
		}
	}
	const storage = "/var/lib/firecracker-builder/storage"
	// fail closed: Assert*, never Condition*
	if !contains(u.Values("Unit", "RequiresMountsFor"), storage) || !contains(u.Values("Unit", "AssertPathIsMountPoint"), storage) {
		t.Error("storage mount guard missing")
	}
	if len(u.Values("Unit", "ConditionPathIsMountPoint"))+len(u.Values("Unit", "ConditionPathExists")) != 0 {
		t.Error("Condition* would silently skip")
	}
	if !contains(u.Values("Unit", "AssertPathExists"), "/dev/kvm") {
		t.Error("kvm assertion missing")
	}
	if !contains(u.Values("Service", "BindPaths"), storage+"/vms/work:/work") {
		t.Error("work bind missing")
	}
	eq(t, u.Values("Service", "Restart"), "no")
	eq(t, u.Values("Service", "MemoryMax"), "768M")
	eq(t, u.Values("Service", "User"), "root")
	mode, _ := strconv.ParseUint(u.Values("Service", "RuntimeDirectoryMode")[0], 8, 32)
	if mode&0o022 != 0 {
		t.Error("slot directories must not be group/other writable")
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestNetworkUnitContract(t *testing.T) {
	u := renderedUnit(t, "firecracker-vm-network@.service")
	eq(t, u.Values("Service", "Type"), "oneshot")
	eq(t, u.Values("Service", "RemainAfterExit"), "yes")
	if len(u.Values("Service", "RootDirectory")) != 0 {
		t.Error("network unit must run in the real filesystem")
	}
	eq(t, u.Values("Service", "ExecStart"), "/usr/local/libexec/fc-vm-network up %i")
	eq(t, u.Values("Service", "ExecStop"), "/usr/local/libexec/fc-vm-network down %i")
	eq(t, u.Values("Service", "ExecReload"), "/usr/local/libexec/fc-vm-network refresh %i")
}

func TestPrivilegeBoundaryIsAGroupRestrictedSocket(t *testing.T) {
	s := renderedUnit(t, "fc-vm-helper.socket")
	eq(t, s.Values("Socket", "SocketMode"), "0660")
	eq(t, s.Values("Socket", "SocketUser"), "root")
	eq(t, s.Values("Socket", "SocketGroup"), "vmusers")
	eq(t, s.Values("Socket", "Accept"), "yes")
	eq(t, s.Values("Socket", "ListenStream"), vmhelper.SocketPath)
	h := renderedUnit(t, "fc-vm-helper@.service")
	eq(t, h.Values("Service", "StandardInput"), "socket")
	eq(t, h.Values("Service", "ExecStart"), "/usr/local/libexec/fc-vm-helper")
	eq(t, h.Values("Service", "ReadWritePaths"), "/run/firecracker-vms /run/firecracker-vms-net")
	eq(t, h.Values("Service", "ProtectSystem"), "strict")
	eq(t, h.Values("Service", "RestrictAddressFamilies"), "AF_UNIX AF_NETLINK")
}

func TestNoUnitUsesSudoOrTheLegacyService(t *testing.T) {
	o := staged(t)
	plan, _ := o.Plan()
	for _, e := range plan {
		if e.Data == nil || strings.Contains(e.Path, "/examples/") {
			continue
		}
		var code []string
		for _, l := range strings.Split(string(e.Data), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "#") {
				code = append(code, l)
			}
		}
		text := strings.Join(code, "\n")
		for _, bad := range []string{"firecracker-builder@", "sudo", "ask-exec"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s contains %q", e.Path, bad)
			}
		}
	}
}

func TestRuntimeTreeIsRootOwnedAndOnlyTheLockIsClientWritable(t *testing.T) {
	o := staged(t)
	plan, _ := o.Plan()
	var text string
	for _, e := range plan {
		if strings.HasSuffix(e.Path, "firecracker-vms.conf") {
			text = string(e.Data)
		}
	}
	var rows [][]string
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "#") {
			rows = append(rows, strings.Fields(l))
		}
	}
	if len(rows) != 2 {
		t.Fatalf("nothing else may be client-writable: %v", rows)
	}
	eq(t, rows[0][:5], "d", "/run/firecracker-vms", "0755", "root", "root")
	eq(t, rows[1][:5], "f", "/run/firecracker-vms/.backend.lock", "0660", "root", "vmusers")
}

func TestExampleConfigIsConsistentWithUnitsAndPool(t *testing.T) {
	read := func(rel string) []byte {
		b, err := os.ReadFile(filepath.Join("../../deployment", rel))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var cfg struct {
		GuestPrefix     string `json:"guest_prefix"`
		MaxVMs          int    `json:"max_vms"`
		PoolConf        string `json:"pool_conf"`
		StorageMount    string `json:"storage_mount"`
		WorkRoot        string `json:"work_root"`
		ImagesDir       string `json:"images_dir"`
		UnitMemLimitMiB int    `json:"unit_mem_limit_mib"`
		MaxTotalMemMiB  int    `json:"max_total_mem_mib"`
		MaxTotalVCPU    int    `json:"max_total_vcpu"`
		VMMOverheadMiB  int    `json:"vmm_overhead_mib"`
		Profiles        []struct {
			VCPU   int `json:"vcpu"`
			MemMiB int `json:"mem_mib"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(read("vmapi/backend.local.example.json"), &cfg); err != nil {
		t.Fatal(err)
	}
	pool, err := vmhelper.ParsePool(read("multivm/pool.conf.example"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GuestPrefix != pool.Prefix || cfg.MaxVMs > pool.MaxSlots || cfg.PoolConf != DefaultPoolConf || cfg.StorageMount != DefaultStorageRoot {
		t.Errorf("example backend and pool disagree: %+v %+v", cfg, pool)
	}
	for _, p := range []string{cfg.WorkRoot, cfg.ImagesDir} {
		if !strings.HasPrefix(p, cfg.StorageMount+"/") {
			t.Errorf("%s outside storage mount", p)
		}
	}
	u := renderedUnit(t, "firecracker-vm@.service")
	if !contains(u.Values("Service", "BindPaths"), cfg.WorkRoot+":/work") {
		t.Error("unit does not bind the configured work root")
	}
	eq(t, u.Values("Service", "MemoryMax"), strconv.Itoa(cfg.UnitMemLimitMiB)+"M")
	for _, p := range cfg.Profiles {
		if p.MemMiB+cfg.VMMOverheadMiB > cfg.UnitMemLimitMiB || p.MemMiB > cfg.MaxTotalMemMiB {
			t.Errorf("profile exceeds unit memory: %+v", p)
		}
		if cfg.MaxVMs*p.MemMiB > cfg.MaxTotalMemMiB || cfg.MaxTotalVCPU < cfg.MaxVMs*p.VCPU {
			t.Errorf("admission cannot hold max_vms guests: %+v", p)
		}
	}
	if cfg.MaxTotalMemMiB > 2048 {
		t.Error("example exceeds the shared slice budget")
	}
}

// --- preflight and setup ---------------------------------------------------------

func fakeHost(t *testing.T) Host {
	t.Helper()
	h := RealHost()
	h.LookPath = func(string) (string, error) { return "/usr/bin/x", nil }
	h.LookGroup = func(string) error { return nil }
	return h
}

func TestPreflightReportsActionableFailures(t *testing.T) {
	dir := t.TempDir()
	h := fakeHost(t)
	h.LookPath = func(n string) (string, error) {
		if n == "conntrack" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + n, nil
	}
	h.LookGroup = func(string) error { return errors.New("no such group") }
	h.ReadFile = func(p string) ([]byte, error) {
		if p == "/proc/sys/net/ipv4/ip_forward" {
			return []byte("0\n"), nil
		}
		return nil, os.ErrNotExist
	}
	o := Options{ClientGroup: "nogroup2", StorageRoot: dir, AssetDir: dir + "/assets"}
	cs := Preflight(context.Background(), o, dir+"/pool.conf", h)
	out := Format(cs)
	if !Failed(cs) {
		t.Fatal("expected failures")
	}
	for _, want := range []string{"conntrack", "IPv4 forwarding is disabled", "nogroup2", "pool.conf", "not a mount point", "--firecracker/--kernel"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "fix:") {
		t.Error("no remedies printed")
	}
}

func TestPreflightPassesOnAReadyHost(t *testing.T) {
	// /proc is a real mount point on any Linux host, so it stands in for the volume.
	dir := t.TempDir()
	pool := filepath.Join(dir, "pool.conf")
	if err := os.WriteFile(pool, []byte("GUEST_PREFIX=10.213\nMAX_SLOTS=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := fakeHost(t)
	real := h.ReadFile
	h.ReadFile = func(p string) ([]byte, error) {
		if p == "/proc/sys/net/ipv4/ip_forward" {
			return []byte("1\n"), nil
		}
		return real(p)
	}
	st := h.Stat
	h.Stat = func(p string) (fs.FileInfo, error) {
		switch p {
		case "/dev/kvm", "/dev/net/tun":
			return st("/dev/null")
		case "/run/systemd/system":
			return st("/proc")
		}
		return st(p)
	}
	h.Runner = routes{"default via 192.168.1.1 dev eth0\n"}
	o := Options{StorageRoot: "/proc", AssetDir: dir, Firecracker: "x", Kernel: "y"}
	for _, c := range Preflight(context.Background(), o, pool, h) {
		if c.Status == Fail && !strings.Contains(c.Name, "pool-owner") {
			t.Errorf("unexpected failure %+v", c)
		}
	}
}

func TestPreflightDetectsSubnetCollision(t *testing.T) {
	dir := t.TempDir()
	pool := filepath.Join(dir, "pool.conf")
	_ = os.WriteFile(pool, []byte("GUEST_PREFIX=10.213\nMAX_SLOTS=2\n"), 0o644)
	h := fakeHost(t)
	h.Runner = routes{"default via 192.168.1.1 dev eth0\n10.213.1.0/24 dev tun0\n"}
	cs := Preflight(context.Background(), Options{StorageRoot: dir, AssetDir: dir}, pool, h)
	if !strings.Contains(Format(cs), "slot 1") || !strings.Contains(Format(cs), "GUEST_PREFIX") {
		t.Fatalf("collision not reported:\n%s", Format(cs))
	}
}

type routes struct{ out string }

func (r routes) Run(_ context.Context, _ string, name string, a ...string) (vmnetResult, error) {
	if name == "ip" && len(a) > 1 && a[1] == "route" {
		return vmnetResult{Stdout: r.out}, nil
	}
	return vmnetResult{}, nil
}

func TestSetupCreatesOnlyMissingEmptyDirectories(t *testing.T) {
	o := staged(t)
	o.Euid = func() int { return 1000 }
	existing := filepath.Join(o.DestDir, "/var/lib/firecracker-builder/storage/vms/images")
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(existing, "base.ext4"), []byte("img"), 0o600)
	var out strings.Builder
	so := SetupOptions{Options: o, User: currentUser(t), Out: &out}
	if err := Setup(context.Background(), so); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.DestDir, JailRoot)); err == nil {
		t.Fatal("dry run created directories")
	}
	so.Apply = true
	out.Reset()
	if err := Setup(context.Background(), so); err != nil {
		t.Fatal(err)
	}
	for p, mode := range map[string]fs.FileMode{
		JailRoot + "/work": 0o755, JailRoot + "/run/slot": 0o755, "/etc/firecracker-vms": 0o755,
		"/var/lib/firecracker-builder/storage/vms/work": 0o770,
	} {
		fi, err := os.Stat(filepath.Join(o.DestDir, p))
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", p, err, fi)
		}
	}
	if fi, _ := os.Stat(existing); fi.Mode().Perm() != 0o700 {
		t.Error("existing directory mode was changed")
	}
	if b, _ := os.ReadFile(filepath.Join(existing, "base.ext4")); string(b) != "img" {
		t.Error("existing image touched")
	}
	if _, err := os.Stat(filepath.Join(o.DestDir, "/etc/firecracker-vms/pool.conf")); err == nil {
		t.Fatal("setup must never write pool.conf")
	}
	if !strings.Contains(out.String(), "left unchanged") {
		t.Errorf("existing dir not reported:\n%s", out.String())
	}
}

func TestEnableOnlyEnablesTheHelperSocket(t *testing.T) {
	var calls []string
	run := recorder(func(args []string) { calls = append(calls, strings.Join(args, " ")) })
	o := Options{Euid: func() int { return 0 }}
	var out strings.Builder
	if err := Enable(context.Background(), o, false, &out, run); err != nil || len(calls) != 0 {
		t.Fatalf("dry run: err=%v calls=%v", err, calls)
	}
	if err := Enable(context.Background(), o, true, &out, run); err != nil {
		t.Fatal(err)
	}
	eq(t, calls, "systemctl daemon-reload", "systemctl enable --now fc-vm-helper.socket")
	o.Euid = func() int { return 1000 }
	if err := Enable(context.Background(), o, true, &out, run); err == nil {
		t.Fatal("enable without root accepted")
	}
}
