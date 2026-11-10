package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote/pkitest"
)

func TestParseConfigRejectsUnsafeShapes(t *testing.T) {
	tls := `{"listen":"127.0.0.1:0","ca":"/a","cert":"/b","key":"/c"}`
	localBackend := `"backend":{"kind":"local","local":{"work_root":"/w","images_dir":"/i","run_dir":"/r","max_vms":2,"helper":"/usr/local/libexec/fc-vm-helper","ssh_user":"root","storage_mount":"/var/lib/firecracker-builder/storage","max_total_mem_mib":1024,"max_total_vcpu":1,"unit_mem_limit_mib":768,"guest_prefix":"10.213","pool_conf":"/etc/firecracker-vms/pool.conf","profiles":[{"name":"small","vcpu":1,"mem_mib":256,"disk_mib":1024}]}}`
	remoteBackend := `"backend":{"kind":"remote","remote":{"addr":"h:1","server_name":"h","ca":"/a","cert":"/b","key":"/c"}}`
	cases := map[string]struct {
		json string
		ok   bool
	}{
		"backend only":          {`{` + localBackend + `,"serve_backend":` + tls + `}`, true},
		"facade over local":     {`{` + localBackend + `,"serve_facade":` + tls + `}`, true},
		"facade over remote":    {`{` + remoteBackend + `,"serve_facade":` + tls + `}`, true},
		"combined":              {`{` + localBackend + `,"serve_backend":` + tls + `,"serve_facade":` + tls + `}`, true},
		"serves nothing":        {`{` + localBackend + `}`, false},
		"re-exporting remote":   {`{` + remoteBackend + `,"serve_backend":` + tls + `}`, false},
		"missing tls key":       {`{` + localBackend + `,"serve_facade":{"listen":"127.0.0.1:0","ca":"/a","cert":"/b"}}`, false},
		"no storage mount":      {strings.Replace(`{`+localBackend+`,"serve_facade":`+tls+`}`, `"storage_mount":"/var/lib/firecracker-builder/storage",`, "", 1), false},
		"no aggregate caps":     {strings.Replace(`{`+localBackend+`,"serve_facade":`+tls+`}`, `"max_total_mem_mib":1024,`, "", 1), false},
		"no guest prefix":       {strings.Replace(`{`+localBackend+`,"serve_facade":`+tls+`}`, `"guest_prefix":"10.213",`, "", 1), false},
		"no pool conf":          {strings.Replace(`{`+localBackend+`,"serve_facade":`+tls+`}`, `"pool_conf":"/etc/firecracker-vms/pool.conf",`, "", 1), false},
		"unknown field":         {`{` + localBackend + `,"serve_facade":` + tls + `,"insecure":true}`, false},
		"relative helper":       {strings.Replace(`{`+localBackend+`,"serve_facade":`+tls+`}`, "/usr/local/libexec/fc-vm-helper", "helper", 1), false},
		"remote w/o servername": {strings.Replace(`{`+remoteBackend+`,"serve_facade":`+tls+`}`, `"server_name":"h"`, `"server_name":""`, 1), false},
	}
	for name, c := range cases {
		_, err := parseConfig([]byte(c.json))
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", name, err, c.ok)
		}
	}
}

type fakeLauncher struct{}

func (fakeLauncher) StartSlot(context.Context, int) error { return nil }

const invID = "0123456789abcdef0123456789abcdef"

type fakeVMM struct{ run string }

func (v fakeVMM) Info(context.Context, int) (local.VMMInfo, error) {
	return local.VMMInfo{ID: invID, State: "Not started"}, nil
}
func (v fakeVMM) Configure(context.Context, int, local.MachineSpec) error { return nil }
func (v fakeVMM) InstanceStart(context.Context, int) error                { return nil }
func (v fakeVMM) CtrlAltDel(_ context.Context, slot int) error {
	dir := filepath.Join(v.run, fmt.Sprintf("slot-%d", slot))
	os.MkdirAll(dir, 0o755)
	body := fmt.Sprintf(`{"invocation_id":%q,"result":"success","exit_code":"exited","exit_status":"0"}`, invID)
	return os.WriteFile(filepath.Join(dir, invID+".exit.json"), []byte(body), 0o640)
}

func writePKI(t *testing.T, dir, name string, ca *pkitest.CA, server bool) (caF, certF, keyF string) {
	t.Helper()
	var p pkitest.Pair
	if server {
		p = ca.Server(t, "vmd.test")
	} else {
		p = ca.Client(t, "client")
	}
	caF, certF, keyF = filepath.Join(dir, name+".ca"), filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	os.WriteFile(caF, ca.CertPEM, 0o644)
	os.WriteFile(certF, p.CertPEM, 0o644)
	os.WriteFile(keyF, p.KeyPEM, 0o600)
	return
}

// Combined mode: both listeners, separate CAs, real gRPC/mTLS, local backend over
// fake launcher/VMM seams. Not evidence of a real VM.
func TestRunCombinedModeEndToEnd(t *testing.T) {
	d := t.TempDir()
	work, images, run := filepath.Join(d, "work"), filepath.Join(d, "images"), filepath.Join(d, "run")
	for _, p := range []string{work, images, run} {
		os.MkdirAll(p, 0o700)
	}
	os.WriteFile(filepath.Join(images, "base.ext4"), []byte("BASE"), 0o444)
	os.WriteFile(filepath.Join(run, ".backend.lock"), nil, 0o600)
	uid := uint32(os.Getuid())
	os.Chmod(d, 0o755)
	beCA, faCA := pkitest.NewCA(t, "backend-ca"), pkitest.NewCA(t, "facade-ca")
	bsCA, bsCert, bsKey := writePKI(t, d, "bs", beCA, true)
	fsCA, fsCert, fsKey := writePKI(t, d, "fs", faCA, true)
	cfg := &config{
		Backend: backendConfig{Kind: "local", Local: &localConfig{WorkRoot: work, ImagesDir: images, RunDir: run, MaxVMs: 2, Helper: "/bin/true", MaxTotalMemMiB: 1024, MaxTotalVCPU: 4, UnitMemLimitMiB: 768, GuestPrefix: "10.213",
			SSHUser: "root", Profiles: []profileConfig{{Name: "small", VCPU: 1, MemMiB: 256, DiskMiB: 64}}}},
		ServeBackend: &listenConfig{Listen: "127.0.0.1:0", CA: bsCA, Cert: bsCert, Key: bsKey},
		ServeFacade:  &listenConfig{Listen: "127.0.0.1:0", CA: fsCA, Cert: fsCert, Key: fsKey},
	}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var addrs map[string]string
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run_(ctx, cfg, deps{Launcher: fakeLauncher{}, VMM: fakeVMM{run: run}, TrustedOwnerUID: &uid, TrustedAncestorRoot: d}, func(a map[string]string) {
			mu.Lock()
			addrs = a
			mu.Unlock()
			close(ready)
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("vmd did not start")
	}

	cc, cf, ck := writePKI(t, d, "fc", faCA, false)
	fcfg, err := remote.ClientTLSFromFiles(fsCA, cf, ck, "vmd.test")
	if err != nil {
		t.Fatal(err)
	}
	_ = cc
	fc, err := remote.DialFacade(addrs["facade"], fcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()
	vm, err := fc.Create(context.Background(), vmlib.CreateRequest{ID: "a", ImageID: "base", Profile: "small"})
	if err != nil || vm.State != vmlib.StateRunning {
		t.Fatalf("%v %+v", err, vm)
	}
	if img, err := fc.Commit(context.Background(), "a", "golden"); err != nil || img.ID != "golden" {
		t.Fatalf("%v %+v", err, img)
	}
	// Backend listener is reachable only with ITS CA's client cert.
	_, bcf, bck := writePKI(t, d, "bc", beCA, false)
	bcfg, _ := remote.ClientTLSFromFiles(bsCA, bcf, bck, "vmd.test")
	bc, err := remote.DialBackend(addrs["backend"], bcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	if imgs, err := bc.ListImages(context.Background()); err != nil || len(imgs) != 2 {
		t.Fatalf("%v %+v", err, imgs)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("vmd did not stop on context cancel")
	}
}

// Finding 5: the policy we SHIP must actually admit the two concurrent guests the
// integration flow needs (and refuse a third). This loads the real example file.
func TestShippedExamplePolicyAdmitsTwoSimultaneousVMsAndRejectsAThird(t *testing.T) {
	data, err := os.ReadFile("../../deployment/multivm/vmd.example.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	l := *cfg.Backend.Local
	d := t.TempDir()
	os.Chmod(d, 0o755)
	l.WorkRoot, l.ImagesDir, l.RunDir = filepath.Join(d, "work"), filepath.Join(d, "images"), filepath.Join(d, "run")
	l.StorageMount, l.PoolConf = "", "" // host-specific; covered by the backend tests
	for _, p := range []string{l.WorkRoot, l.ImagesDir, l.RunDir} {
		os.MkdirAll(p, 0o700)
	}
	os.WriteFile(filepath.Join(l.RunDir, ".backend.lock"), nil, 0o600)
	os.WriteFile(filepath.Join(l.ImagesDir, "base.ext4"), []byte("BASE"), 0o444)
	uid := uint32(os.Getuid())
	b, err := buildLocal(&l, deps{Launcher: fakeLauncher{}, VMM: fakeVMM{run: l.RunDir}, TrustedOwnerUID: &uid, TrustedAncestorRoot: d})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	prof := l.Profiles[0].Name
	ctx := context.Background()
	for _, id := range []string{"v1", "v2"} {
		if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: id, ImageID: "base", Profile: prof}); err != nil {
			t.Fatalf("the shipped policy refuses concurrent VM %s: %v", id, err)
		}
	}
	if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "v3", ImageID: "base", Profile: prof}); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("a third VM must be refused: %v", err)
	}
}
