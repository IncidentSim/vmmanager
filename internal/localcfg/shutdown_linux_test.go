//go:build linux

package localcfg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
)

func TestShutdownKnownHostsOptIn(t *testing.T) {
	c := &JSONConfig{SSHUser: "root", PersonalSSH: &PersonalSSHConfig{Enabled: true, StoreDir: "/service/creds", MasterKeyFile: "/service/master"}}
	if err := c.validatePersonalSSH(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"relative", "/x/../kh", "/trusted/%h", "/trusted/kh\n"} {
		c.PersonalSSH.ShutdownKnownHostsFile = p
		if c.validatePersonalSSH() == nil {
			t.Fatalf("unsafe path accepted %q", p)
		}
	}
	c.PersonalSSH.ShutdownKnownHostsFile = "/trusted/known_hosts"
	if err := c.validatePersonalSSH(); err != nil {
		t.Fatal(err)
	}
	c.PersonalSSH.Enabled = false
	if c.validatePersonalSSH() == nil {
		t.Fatal("disabled accepted shutdown")
	}
}
func TestShutdownHookWiring(t *testing.T) {
	l, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	personal := personalConfig(t)
	l.PersonalSSH = personal.PersonalSSH
	l.SSHUser = "root"
	seams := Seams{Launcher: fakeLauncher{}, VMM: fakeVMM{}}
	cfg, err := l.Assemble(seams)
	if err != nil || cfg.CooperativeShutdown != nil {
		t.Fatalf("default enabled: %v", err)
	}
	kh := filepath.Join(filepath.Dir(l.PersonalSSH.MasterKeyFile), "known_hosts")
	_, pub, _ := sshaccess.GenerateEd25519()
	os.WriteFile(kh, []byte("172.30.0.2 "+pub+"\n"), 0600)
	l.PersonalSSH.ShutdownKnownHostsFile = kh
	cfg, err = l.Assemble(seams)
	if err != nil || cfg.CooperativeShutdown == nil {
		t.Fatalf("opt-in not wired: %v", err)
	}
	os.Remove(kh)
	if _, err = l.Assemble(seams); err == nil {
		t.Fatal("missing known hosts silently accepted")
	}
}

func TestShutdownKnownHostsValidatedFile(t *testing.T) {
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(d, "known_hosts")
	_, pub, err := sshaccess.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kh, []byte("172.30.0.2 "+pub), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateMasterAncestors(kh); err != nil {
		t.Fatal("ancestry", kh, err)
	}
	if err := validateShutdownKnownHosts(kh); err != nil {
		t.Fatal(err)
	}
	os.Chmod(kh, 0666)
	if validateShutdownKnownHosts(kh) == nil {
		t.Fatal("writable trust accepted")
	}
	os.Chmod(kh, 0600)
	link := filepath.Join(d, "link")
	os.Symlink(kh, link)
	if validateShutdownKnownHosts(link) == nil {
		t.Fatal("symlink trust accepted")
	}
	os.WriteFile(kh, []byte("bad host key"), 0600)
	if validateShutdownKnownHosts(kh) == nil {
		t.Fatal("malformed trust accepted")
	}
	os.WriteFile(kh, nil, 0600)
	if validateShutdownKnownHosts(kh) == nil {
		t.Fatal("empty trust accepted")
	}
}
