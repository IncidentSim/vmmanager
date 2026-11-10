package remote_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	backendv1 "github.com/petruekhin/vibe-firecracker/vmlib/gen/vmlib/backend/v1"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote"
)

func connect(t *testing.T, e *env) *remote.BackendClient {
	t.Helper()
	c, err := dial(t, e, e.ca.CertPEM, e.client, host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// The facade over the REMOTE backend is the same Service used locally.
func TestServiceOverRemoteBackendFullLifecycle(t *testing.T) {
	e := startBackend(t)
	svc, err := vmlib.NewService(connect(t, e))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	vm, err := svc.Create(ctx, vmlib.CreateRequest{ID: "r1", ImageID: "base", Profile: "small"})
	if err != nil || vm.State != vmlib.StateRunning {
		t.Fatalf("%v %+v", err, vm)
	}
	info, err := svc.SSHInfo(ctx, "r1")
	if err != nil || info.Host != "172.16.1.2" || info.Bastion == nil || info.Bastion.User != "jump" {
		t.Fatalf("%v %+v", err, info)
	}
	img, err := svc.Commit(ctx, "r1", "golden")
	if err != nil || img.ID != "golden" {
		t.Fatalf("%v %+v", err, img)
	}
	if _, err := svc.Commit(ctx, "r1", "base"); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("base overwrite over RPC: %v", err)
	}
	for _, id := range []string{"c1", "c2"} {
		if _, err := svc.Create(ctx, vmlib.CreateRequest{ID: id, ImageID: "golden", Profile: "small"}); err == nil {
			continue
		} else if !errors.Is(err, vmlib.ErrResourceExhausted) {
			t.Fatalf("%v", err)
		}
	}
	if res, err := svc.Delete(ctx, "r1"); err != nil || !res.Deleted {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := svc.Get(ctx, "r1"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestRemoteErrorsMapToSentinels(t *testing.T) {
	e := startBackend(t)
	c := connect(t, e)
	ctx := context.Background()
	if _, err := c.GetImage(ctx, "missing"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Errorf("notfound: %v", err)
	}
	if _, err := c.Prepare(ctx, vmlib.PrepareRequest{VMID: "a", ImageID: "base", Profile: "huge"}); !errors.Is(err, vmlib.ErrInvalidArgument) {
		t.Errorf("profile: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := c.Prepare(ctx, vmlib.PrepareRequest{VMID: id, ImageID: "base", Profile: "small"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Prepare(ctx, vmlib.PrepareRequest{VMID: "c", ImageID: "base", Profile: "small"}); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Errorf("cap: %v", err)
	}
	if _, err := c.Prepare(ctx, vmlib.PrepareRequest{VMID: "a", ImageID: "base", Profile: "other"}); err == nil {
		t.Error("conflicting prepare accepted")
	}
}

// Even an authenticated client cannot smuggle paths or odd identifiers.
func TestServerRejectsPathsAndBadIDsBeforeBackend(t *testing.T) {
	e := startBackend(t)
	cfg, _ := remote.ClientTLSFromPEM(e.ca.CertPEM, e.client.CertPEM, e.client.KeyPEM, host)
	conn, err := grpc.Dial(e.addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw := backendv1.NewHostBackendClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bad := []*backendv1.PrepareRequest{
		{VmId: "../../etc", ImageId: "base", Profile: "small"},
		{VmId: "ok", ImageId: "/var/lib/firecracker-builder/work/x.ext4", Profile: "small"},
		{VmId: "ok", ImageId: "base", Profile: "small; reboot"},
		{VmId: "OK", ImageId: "base", Profile: "small"},
		{VmId: "ok", ImageId: "base", Profile: "small", SshPublicKey: "ssh-ed25519 AAA\ncommand=\"x\""},
	}
	for _, r := range bad {
		if _, err := raw.Prepare(ctx, r); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%+v: %v", r, err)
		}
	}
	for _, id := range []string{"", "a/b", "..", "A", "x y"} {
		if _, err := raw.Publish(ctx, &backendv1.PublishRequest{VmId: id, NewImageId: "n"}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("publish %q: %v", id, err)
		}
		if _, err := raw.Release(ctx, &backendv1.VMRef{VmId: id}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("release %q: %v", id, err)
		}
	}
	if e.backend.Prepared != 0 || e.backend.Published != 0 || e.backend.Released != 0 {
		t.Fatal("rejected request reached the backend")
	}
}

func TestWaitExitDeadlineIsNotAnExit(t *testing.T) {
	e := startBackend(t)
	e.backend.AutoExit = false
	c := connect(t, e)
	svc, _ := vmlib.NewService(c)
	if _, err := svc.Create(context.Background(), vmlib.CreateRequest{ID: "w", ImageID: "base", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.WaitExit(ctx, "w"); err == nil {
		t.Fatal("WaitExit returned success without a receipt")
	}
	if _, err := svc.Commit(ctx, "w", "nope"); err == nil || e.backend.Published != 0 {
		t.Fatal("published without receipt over RPC")
	}
	if res, _ := c.Release(context.Background(), "w"); res.Released {
		t.Fatal("released a disk whose exit was not proven")
	}
}

func TestParallelRemoteCreatesRespectServerCap(t *testing.T) {
	e := startBackend(t)
	svc, _ := vmlib.NewService(connect(t, e))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Create(context.Background(), vmlib.CreateRequest{ID: string(rune('a'+i)) + "vm", ImageID: "base", Profile: "small"})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if !errors.Is(err, vmlib.ErrResourceExhausted) {
				t.Errorf("%v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 2 || e.backend.Count() != 2 {
		t.Fatalf("ok=%d leases=%d", ok, e.backend.Count())
	}
}
