// Package hostinstall plans, stages, installs and removes the host artifacts of
// the product (binaries, systemd units, tmpfiles, examples), renders units
// against the chosen paths and checks the host before activation.
//
// Installation only places files. It never starts, enables or reloads anything,
// never edits firewall rules, and never touches configuration, keys, images or
// state: those are created or activated by separate, deliberate commands.
package hostinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Compatibility defaults: the storage volume and asset directory that existing
// hosts already use. Both can be overridden per installation.
const (
	DefaultPrefix      = "/usr/local"
	DefaultSysconfDir  = "/etc"
	DefaultAssetDir    = "/opt/firecracker-builder"
	DefaultStorageRoot = "/var/lib/firecracker-builder/storage"
	DefaultPoolConf    = "/etc/firecracker-vms/pool.conf"
	JailRoot           = "/var/lib/firecracker-vms/jail"
	ManifestRel        = "share/vm-firecracker/install-manifest.json"
)

// Options select what is installed and where.
type Options struct {
	Prefix      string // default /usr/local
	DestDir     string // staging root; empty means the live filesystem
	SysconfDir  string // default /etc
	ClientGroup string // primary group of the unprivileged vmapi user
	AssetDir    string // holds firecracker and vmlinux
	StorageRoot string // existing storage mount that holds vms/{work,images}
	BuildDir    string // directory with the built executables
	Firecracker string // optional static Firecracker executable to install
	Kernel      string // optional kernel image to install
	Force       bool   // replace unmanaged files / remove modified owned files
	// Euid is a seam for tests; nil means os.Geteuid.
	Euid func() int
}

var (
	safePath  = regexp.MustCompile(`^/[A-Za-z0-9_./+-]*$`)
	groupName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

func (o Options) withDefaults() Options {
	if o.Prefix == "" {
		o.Prefix = DefaultPrefix
	}
	if o.SysconfDir == "" {
		o.SysconfDir = DefaultSysconfDir
	}
	if o.AssetDir == "" {
		o.AssetDir = DefaultAssetDir
	}
	if o.StorageRoot == "" {
		o.StorageRoot = DefaultStorageRoot
	}
	if o.Euid == nil {
		o.Euid = os.Geteuid
	}
	return o
}

func (o Options) bindir() string     { return o.Prefix + "/bin" }
func (o Options) libexecdir() string { return o.Prefix + "/libexec" }
func (o Options) unitdir() string    { return o.SysconfDir + "/systemd/system" }
func (o Options) tmpfilesdir() string {
	return o.SysconfDir + "/tmpfiles.d"
}
func (o Options) userunitdir() string { return o.Prefix + "/lib/systemd/user" }
func (o Options) docdir() string      { return o.Prefix + "/share/doc/vm-firecracker" }
func (o Options) manifestPath() string {
	return o.Prefix + "/" + ManifestRel
}

// Validate checks every path and the group name. Values are embedded into unit
// files, so they are restricted to a conservative character set.
func (o Options) Validate() error {
	o = o.withDefaults()
	for name, p := range map[string]string{"prefix": o.Prefix, "sysconfdir": o.SysconfDir, "asset-dir": o.AssetDir, "storage-root": o.StorageRoot} {
		if err := checkAbs(name, p); err != nil {
			return err
		}
	}
	if o.ClientGroup != "" && !groupName.MatchString(o.ClientGroup) {
		return fmt.Errorf("client group %q is not a valid group name", o.ClientGroup)
	}
	if o.DestDir != "" {
		if !filepath.IsAbs(o.DestDir) {
			return fmt.Errorf("DESTDIR %q must be absolute", o.DestDir)
		}
		if strings.ContainsRune(o.DestDir, 0) {
			return errors.New("DESTDIR contains NUL")
		}
	}
	return nil
}

func checkAbs(name, p string) error {
	if !safePath.MatchString(p) || filepath.Clean(p) != p || p == "/" {
		return fmt.Errorf("%s %q must be a clean absolute path of [A-Za-z0-9_./+-] and not /", name, p)
	}
	return nil
}
