package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote/pkitest"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

func digestOf(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }

// TestExecutableControlFlow runs the REAL vmapi binary as a child process.
// Boundary (be honest): the VM backend behind the mTLS facade is the in-memory
// vmlibtest fake - there is no Firecracker, KVM, guest, SSH login or real disk.
// Real parts: the vmapi process, flags/config loading, mutual-TLS gRPC to the
// facade (test CA), DBOS with SQLite in a test state dir, the HTTP API and the
// public controlclient SDK.
func TestExecutableControlFlow(t *testing.T) {
	dir := t.TempDir()
	ca := pkitest.NewCA(t, "smoke")
	server := ca.Server(t, "facade.test")
	client := ca.Client(t, "api")
	tlsCfg, e := remote.ServerTLSFromPEM(ca.CertPEM, server.CertPEM, server.KeyPEM)
	if e != nil {
		t.Fatal(e)
	}
	grpc := remote.NewGRPCServer(tlsCfg)
	svc, e := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	if e != nil {
		t.Fatal(e)
	}
	remote.RegisterFacade(grpc, svc)
	gl, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	go grpc.Serve(gl)
	defer grpc.Stop()
	for name, b := range map[string][]byte{"ca": ca.CertPEM, "cert": client.CertPEM, "key": client.KeyPEM} {
		if e = os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := l.Addr().String()
	l.Close()
	cfg := map[string]any{
		"listen_addr": addr, "state_dir": filepath.Join(dir, "state"),
		"allowed_user_ids": []int64{11, 22}, "shared_image_ids": []string{"ubuntu"}, "max_vms": 2, "max_vms_per_user": 1,
		"credentials": []any{
			map[string]any{"sha256": digestOf("test-only-user-11"), "user_id": 11},
			map[string]any{"sha256": digestOf("test-only-service"), "service": true},
		},
		"facade": map[string]string{"addr": gl.Addr().String(), "ca_file": filepath.Join(dir, "ca"), "cert_file": filepath.Join(dir, "cert"), "key_file": filepath.Join(dir, "key"), "server_name": "facade.test"},
	}
	b, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "api.json")
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	binary := filepath.Join(dir, "vmapi")
	bctx, bcancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer bcancel()
	if out, e := exec.CommandContext(bctx, "go", "build", "-o", binary, ".").CombinedOutput(); e != nil {
		t.Fatalf("build %v %s", e, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-config", path)
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	exited := false
	defer func() {
		if !exited {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	url := "http://" + addr
	hc := &http.Client{Timeout: 2 * time.Second}
	ready := false
	for until := time.Now().Add(10 * time.Second); time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
		if p, e := hc.Get(url + "/healthz"); e == nil {
			p.Body.Close()
			if p.StatusCode == 200 {
				ready = true
				break
			}
		}
	}
	if !ready {
		t.Fatalf("API did not become healthy: %s", logs.String())
	}

	// Authentication through the public SDK.
	bad, _ := controlclient.NewUser(url, "wrong-token", 11, hc)
	if _, e = bad.ListVMs(ctx, 11); !isAPI(e, 401) {
		t.Fatalf("bad bearer: %v", e)
	}
	user, _ := controlclient.NewUser(url, "test-only-user-11", 11, hc)
	svcCli, _ := controlclient.New(url, "test-only-service", hc)
	if _, e = svcCli.ListVMs(ctx, 33); !isAPI(e, 403) {
		t.Fatalf("service acting as non-allowlisted user: %v", e)
	}
	if _, e = svcCli.ListVMs(ctx, 22); e != nil {
		t.Fatal(e)
	}
	wait := func(c *controlclient.Client, u int64, id string) controlclient.Operation {
		t.Helper()
		o, e := c.Wait(ctx, u, id, 20*time.Millisecond)
		if e != nil {
			t.Fatalf("operation %s: %v (%+v)", id, e, o)
		}
		return o
	}

	// Create with explicit resources (public SDK, real HTTP, real DBOS).
	res := &controlclient.Resources{VCPU: 1, MemoryMiB: 256}
	created, e := user.Create(ctx, 11, "smoke-create", controlclient.CreateRequest{Name: "smoke", ImageID: "ubuntu", Resources: res})
	if e != nil {
		t.Fatal(e)
	}
	again, e := user.Create(ctx, 11, "smoke-create", controlclient.CreateRequest{Name: "smoke", ImageID: "ubuntu", Resources: res})
	if e != nil || again.OperationID != created.OperationID || again.VMID != created.VMID {
		t.Fatalf("idempotent replay %v %+v", e, again)
	}
	if _, e = user.Create(ctx, 11, "smoke-create", controlclient.CreateRequest{Name: "different", ImageID: "ubuntu"}); !isAPI(e, 409) {
		t.Fatalf("same key different body: %v", e)
	}
	wait(user, 11, created.OperationID)
	vm, e := user.VM(ctx, 11, created.VMID)
	if e != nil || vm.Name != "smoke" || vm.Resources != *res || vm.Phase != "active" {
		t.Fatalf("vm %v %+v", e, vm)
	}
	// Another owner cannot see or touch it: identical 404.
	if _, e = svcCli.VM(ctx, 22, created.VMID); !isAPI(e, 404) {
		t.Fatalf("foreign get: %v", e)
	}
	if _, e = svcCli.Delete(ctx, 22, created.VMID, "evil"); !isAPI(e, 404) {
		t.Fatalf("foreign delete: %v", e)
	}
	// Per-user quota is 1.
	if _, e = user.Create(ctx, 11, "second", controlclient.CreateRequest{ImageID: "ubuntu"}); !isAPI(e, 429) {
		t.Fatalf("quota: %v", e)
	}
	// Current local backend does not accept a caller SSH key.
	if _, e = svcCli.Create(ctx, 22, "key", controlclient.CreateRequest{ImageID: "ubuntu", SSHPublicKey: "ssh-ed25519 AAAA"}); !isAPI(e, 422) {
		t.Fatalf("ssh key: %v", e)
	}
	// SSH metadata (public only).
	ssh, e := user.SSH(ctx, 11, created.VMID)
	if e != nil || ssh.Host == "" || ssh.Port == 0 || ssh.User == "" {
		t.Fatalf("ssh %v %+v", e, ssh)
	}

	// Cold commit publishes an owner-private image; the source stays occupied.
	commit, e := user.Commit(ctx, 11, created.VMID, "smoke-commit", "derived")
	if e != nil || commit.ImageID == "" {
		t.Fatalf("commit %v %+v", e, commit)
	}
	wait(user, 11, commit.OperationID)
	img, e := user.Image(ctx, 11, commit.ImageID)
	if e != nil || img.Shared || img.Name != "derived" {
		t.Fatalf("image %v %+v", e, img)
	}
	if _, e = svcCli.Image(ctx, 22, commit.ImageID); !isAPI(e, 404) {
		t.Fatalf("foreign image: %v", e)
	}
	if _, e = svcCli.Create(ctx, 22, "foreign-boot", controlclient.CreateRequest{ImageID: commit.ImageID}); !isAPI(e, 404) {
		t.Fatalf("foreign boot from private image: %v", e)
	}
	committed, _ := user.VM(ctx, 11, created.VMID)
	if committed.Phase != "committed" {
		t.Fatalf("source phase %q", committed.Phase)
	}
	if _, e = user.Create(ctx, 11, "from-image-early", controlclient.CreateRequest{ImageID: commit.ImageID}); !isAPI(e, 429) {
		t.Fatalf("committed source must keep occupying capacity: %v", e)
	}
	del, e := user.Delete(ctx, 11, created.VMID, "smoke-delete")
	if e != nil {
		t.Fatal(e)
	}
	wait(user, 11, del.OperationID)
	if _, e = user.VM(ctx, 11, created.VMID); !isAPI(e, 404) {
		t.Fatalf("deleted VM still visible: %v", e)
	}

	// Create from the derived image, then verify the image is protected.
	second, e := user.Create(ctx, 11, "smoke-create-2", controlclient.CreateRequest{Name: "from-image", ImageID: commit.ImageID})
	if e != nil {
		t.Fatal(e)
	}
	wait(user, 11, second.OperationID)
	if _, e = user.DeleteImage(ctx, 11, commit.ImageID, "img-in-use"); !isAPI(e, 409) {
		t.Fatalf("image used by VM must not be deletable: %v", e)
	}
	del, e = user.Delete(ctx, 11, second.VMID, "smoke-delete-2")
	if e != nil {
		t.Fatal(e)
	}
	wait(user, 11, del.OperationID)
	di, e := user.DeleteImage(ctx, 11, commit.ImageID, "smoke-delete-image")
	if e != nil {
		t.Fatal(e)
	}
	wait(user, 11, di.OperationID)
	if _, e = user.Image(ctx, 11, commit.ImageID); !isAPI(e, 404) {
		t.Fatalf("deleted image still visible: %v", e)
	}
	if _, e = user.DeleteImage(ctx, 11, "ubuntu", "no-shared"); !isAPI(e, 409) {
		t.Fatalf("shared base must not be deletable: %v", e)
	}

	// Graceful shutdown, then a restart on the same state proves durability and
	// that the exclusive lifetime lock was released.
	if e = cmd.Process.Signal(syscall.SIGTERM); e != nil {
		t.Fatal(e)
	}
	werr := cmd.Wait()
	exited = true
	if werr != nil {
		t.Fatalf("shutdown %v %s", werr, logs.String())
	}
	if bytes.Contains(logs.Bytes(), []byte("test-only")) {
		t.Fatal("bearer leaked into logs")
	}

	// Restart on the same state: lifetime lock was released, old operations are
	// still queryable, and deleted resources stay deleted.
	cmd2 := exec.CommandContext(ctx, binary, "-config", path)
	var logs2 bytes.Buffer
	cmd2.Stdout, cmd2.Stderr = &logs2, &logs2
	if e = cmd2.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cmd2.Process.Kill(); cmd2.Wait() }()
	ready = false
	for until := time.Now().Add(10 * time.Second); time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
		if p, e := hc.Get(url + "/healthz"); e == nil {
			p.Body.Close()
			ready = p.StatusCode == 200
			if ready {
				break
			}
		}
	}
	if !ready {
		t.Fatalf("restart not healthy: %s", logs2.String())
	}
	if o, e := user.Operation(ctx, 11, created.OperationID); e != nil || o.Status != "succeeded" {
		t.Fatalf("operation lost across restart: %v %+v", e, o)
	}
	if vms, e := user.ListVMs(ctx, 11); e != nil || len(vms) != 0 {
		t.Fatalf("after restart %v %+v", e, vms)
	}
	if e = cmd2.Process.Signal(syscall.SIGTERM); e != nil {
		t.Fatal(e)
	}
	if e = cmd2.Wait(); e != nil {
		t.Fatalf("second shutdown %v %s", e, logs2.String())
	}
}

func isAPI(e error, status int) bool {
	var a *controlclient.APIError
	return errors.As(e, &a) && a.Status == status
}
