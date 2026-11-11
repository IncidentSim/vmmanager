//go:build linux

package localcfg

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

func personalConfig(t *testing.T) *JSONConfig {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	master := filepath.Join(d, "master.key")
	if err := sshaccess.GenerateMasterKeyFile(master); err != nil {
		t.Fatal(err)
	}
	return &JSONConfig{SSHUser: "root", PersonalSSH: &PersonalSSHConfig{Enabled: true, StoreDir: filepath.Join(d, "credentials"), MasterKeyFile: master}}
}
func TestPersonalSSHExplicitOptInAndStrictConfig(t *testing.T) {
	l, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if a, err := l.openAccess(); err != nil || a != nil {
		t.Fatal("legacy default enabled personal access", err)
	}
	for _, body := range []string{
		`{"enabled":false,"store_dir":"/private"}`,
		`{"enabled":true,"store_dir":"relative","master_key_file":"/master"}`,
		`{"enabled":true,"store_dir":"/private","master_key_file":"/private/master"}`,
		`{"enabled":true,"store_dir":"/private","master_key_file":"/master","private_key":"secret"}`,
		`{"enabled":true,"store_dir":"/private","master_key_file":"/master","master_key":"secret"}`,
	} {
		data := strings.TrimSuffix(strings.TrimSpace(good), "}") + `,"personal_ssh":` + body + `}`
		if _, err = Parse([]byte(data)); err == nil {
			t.Fatalf("unsafe config accepted: %s", body)
		}
	}
}
func TestPersonalSSHStoreValidatesMasterKey(t *testing.T) {
	for _, tc := range []string{"mode", "size", "symlink", "ancestor-symlink", "ancestor-writable", "missing"} {
		t.Run(tc, func(t *testing.T) {
			l := personalConfig(t)
			key := l.PersonalSSH.MasterKeyFile
			switch tc {
			case "mode":
				os.Chmod(key, 0644)
			case "size":
				os.WriteFile(key, []byte("bad"), 0600)
			case "symlink":
				os.Rename(key, key+".real")
				os.Symlink(key+".real", key)
			case "ancestor-symlink":
				dir := filepath.Join(filepath.Dir(key), "link")
				os.Symlink(filepath.Dir(key), dir)
				l.PersonalSSH.MasterKeyFile = filepath.Join(dir, "master.key")
			case "ancestor-writable":
				os.Chmod(filepath.Dir(key), 0777)
			case "missing":
				os.Remove(key)
			}
			if _, err := l.openAccess(); err == nil {
				t.Fatal("unsafe/missing master key accepted")
			}
		})
	}
}
func TestBuildWithAccessReturnsSharedStoreAndTruthfulCapabilities(t *testing.T) {
	l := personalConfig(t)
	d := t.TempDir()
	os.Chmod(d, 0755)
	l.WorkRoot, l.ImagesDir, l.RunDir = filepath.Join(d, "work"), filepath.Join(d, "images"), filepath.Join(d, "run")
	l.MaxVMs = 2
	l.MaxTotalMemMiB = 1024
	l.MaxTotalVCPU = 2
	l.UnitMemLimitMiB = 768
	l.GuestPrefix = "10.213"
	l.Profiles = []Profile{{Name: "small", VCPU: 1, MemMiB: 256, DiskMiB: 64}}
	for _, p := range []string{l.WorkRoot, l.ImagesDir, l.RunDir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(l.RunDir, ".backend.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Getuid())
	s := Seams{Launcher: fakeLauncher{}, VMM: fakeVMM{}, TrustedOwnerUID: &uid, TrustedAncestorRoot: d}
	b, creds, err := BuildWithAccess(l, s)
	if err != nil {

		t.Fatal(err)
	}
	defer b.Close()
	if creds == nil {
		t.Fatal("no credential provider")
	}
	caps, err := b.Capabilities(context.Background())
	if err != nil || caps.SSHAuthMode != vmlib.AuthProvisionPublicKey {
		t.Fatal("capabilities not provisioned", caps, err)
	}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE KEY") || strings.Contains(string(raw), "owner_private") {
		t.Fatal("config leaks secret")
	}
	store := creds.(*sshaccess.Store)
	if _, err = store.Ensure("shared-store", ""); err != nil {
		t.Fatal(err)
	}
	access, err := l.openAccess()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := access.(*accessHooks).store.Public("shared-store")
	if err != nil || !pub.OwnerGenerated {
		t.Fatal("not the same persistent store", err)
	}
}
