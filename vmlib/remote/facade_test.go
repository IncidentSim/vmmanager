package remote_test

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote/pkitest"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

type facadeEnv struct {
	ca     *pkitest.CA
	client pkitest.Pair
	addr   string
}

func serveFacade(t *testing.T, svc vmlib.Lifecycle, opts ...remote.FacadeOption) *facadeEnv {
	t.Helper()
	ca := pkitest.NewCA(t, "facade-ca") // distinct trust domain from the backend's
	srvPair := ca.Server(t, host)
	cfg, err := remote.ServerTLSFromPEM(ca.CertPEM, srvPair.CertPEM, srvPair.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := remote.NewGRPCServer(cfg)
	remote.RegisterFacade(srv, svc, opts...)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return &facadeEnv{ca: ca, client: ca.Client(t, "automation"), addr: lis.Addr().String()}
}

func facadeClient(t *testing.T, f *facadeEnv) *remote.FacadeClient {
	t.Helper()
	cfg, err := remote.ClientTLSFromPEM(f.ca.CertPEM, f.client.CertPEM, f.client.KeyPEM, host)
	if err != nil {
		t.Fatal(err)
	}
	c, err := remote.DialFacade(f.addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// exercise runs the user's flow against ANY vmlib.Lifecycle implementation.
func exercise(t *testing.T, l vmlib.Lifecycle) {
	t.Helper()
	ctx := context.Background()
	vm, err := l.Create(ctx, vmlib.CreateRequest{ID: "e1", ImageID: "base", Profile: "small", Labels: map[string]string{"k": "v"}, SSHKeyReference: "mykey"})
	if err != nil || vm.State != vmlib.StateRunning || vm.Labels["k"] != "v" {
		t.Fatalf("create: %v %+v", err, vm)
	}
	info, err := l.SSHInfo(ctx, "e1")
	if err != nil || info.Host == "" || info.ProxyJump() != "jump@bastion.example.test" || info.KeyReference != "mykey" {
		t.Fatalf("ssh: %v %+v", err, info)
	}
	if _, err := l.Create(ctx, vmlib.CreateRequest{ID: "e1", ImageID: "base", Profile: "small"}); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("dup: %v", err)
	}
	if _, err := l.Create(ctx, vmlib.CreateRequest{ID: "../e", ImageID: "base", Profile: "small"}); !errors.Is(err, vmlib.ErrInvalidArgument) {
		t.Fatalf("invalid: %v", err)
	}
	if vm, err := l.Stop(ctx, "e1"); err != nil || vm.State != vmlib.StateStopped {
		t.Fatalf("stop: %v %+v", err, vm)
	}
	if vm, err := l.Stop(ctx, "e1"); err != nil || vm.State != vmlib.StateStopped {
		t.Fatalf("stop again: %v %+v", err, vm)
	}
	img, err := l.Commit(ctx, "e1", "golden")
	if err != nil || img.ID != "golden" {
		t.Fatalf("commit: %v %+v", err, img)
	}
	if _, err := l.Commit(ctx, "e1", "base"); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("overwrite: %v", err)
	}
	for _, id := range []string{"e2", "e3"} {
		if _, err := l.Create(ctx, vmlib.CreateRequest{ID: id, ImageID: "golden", Profile: "small"}); err != nil {
			t.Fatalf("start from image %s: %v", id, err)
		}
	}
	list, err := l.List(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %v %+v", err, list)
	}
	if _, err := l.Create(ctx, vmlib.CreateRequest{ID: "e4", ImageID: "golden", Profile: "small"}); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("cap: %v", err)
	}
	if imgs, err := l.ListImages(ctx); err != nil || len(imgs) != 2 {
		t.Fatalf("images: %v %+v", err, imgs)
	}
	for _, id := range []string{"e1", "e2", "e3", "e3"} {
		if res, err := l.Delete(ctx, id); err != nil || !res.Deleted {
			t.Fatalf("delete %s: %v %+v", id, err, res)
		}
	}
	if _, err := l.Get(ctx, "e1"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestComposition_LocalServiceOverFakeBackend(t *testing.T) {
	svc, _ := vmlib.NewService(vmlibtest.New(3, "base"))
	exercise(t, svc)
}

func TestComposition_FacadeClientOverFacadeServerOverLocalService(t *testing.T) {
	svc, _ := vmlib.NewService(vmlibtest.New(3, "base"))
	exercise(t, facadeClient(t, serveFacade(t, svc)))
}

// Combined: client -> facade(mTLS #1) -> Service -> remote backend(mTLS #2) -> host.
func TestComposition_FacadeOverRemoteBackendWithSeparateTrustDomains(t *testing.T) {
	be := startBackend(t)
	c3, _ := be.backend.Capabilities(context.Background())
	c3.MaxVMs = 3
	be.backend.SetCapabilities(c3)
	bc := connect(t, be)
	svc, _ := vmlib.NewService(bc)
	f := serveFacade(t, svc)
	exercise(t, facadeClient(t, f))

	// The facade CA's client certificate must not open the backend listener,
	// and the backend CA's client certificate must not open the facade listener.
	facadeCert := f.client
	if c, err := dial(t, be, be.ca.CertPEM, facadeCert, host); err == nil {
		defer c.Close()
		if caps(c) == nil {
			t.Fatal("facade credential accepted by backend listener")
		}
	}
	cfg, _ := remote.ClientTLSFromPEM(f.ca.CertPEM, be.client.CertPEM, be.client.KeyPEM, host)
	fc, err := remote.DialFacade(f.addr, cfg)
	if err == nil {
		defer fc.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := fc.List(ctx); err == nil {
			t.Fatal("backend credential accepted by facade listener")
		}
	}
}

func TestFacadeServerRejectsUnauthenticatedClient(t *testing.T) {
	svc, _ := vmlib.NewService(vmlibtest.New(1, "base"))
	f := serveFacade(t, svc)
	rogue := pkitest.NewCA(t, "rogue")
	p := rogue.Client(t, "x")
	cfg, _ := remote.ClientTLSFromPEM(f.ca.CertPEM, p.CertPEM, p.KeyPEM, host)
	c, err := remote.DialFacade(f.addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.List(ctx); err == nil {
		t.Fatal("untrusted client accepted")
	}
}

// A client-side cancelled Create RPC must not kill a VM that already runs.
func TestFacadeClientContextEndDoesNotKillVM(t *testing.T) {
	be := vmlibtest.New(1, "base")
	svc, _ := vmlib.NewService(be)
	c := facadeClient(t, serveFacade(t, svc))
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := c.Create(ctx, vmlib.CreateRequest{ID: "k", ImageID: "base", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	vm, err := c.Get(context.Background(), "k")
	if err != nil || vm.State != vmlib.StateRunning || be.Released != 0 {
		t.Fatalf("%v %+v", err, vm)
	}
}

// The portable client must not drag in the Firecracker SDK or the local backend.
func TestRemotePackageHasNoKVMDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/petruekhin/vibe-firecracker/vmlib/remote").CombinedOutput()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, bad := range []string{"firecracker-go-sdk", "vmlib/local"} {
		if strings.Contains(string(out), bad) {
			t.Errorf("vmlib/remote depends on %s", bad)
		}
	}
}

func TestFacadeKeyReferenceAndLabelsSurviveTheWire(t *testing.T) {
	svc, _ := vmlib.NewService(vmlibtest.New(2, "base"))
	f := serveFacade(t, svc)
	c := facadeClient(t, f)
	ctx := context.Background()
	if _, err := c.Create(ctx, vmlib.CreateRequest{ID: "k1", ImageID: "base", Profile: "small",
		Labels: map[string]string{"team": "x"}, SSHKeyReference: "laptop-ed25519"}); err != nil {
		t.Fatal(err)
	}
	// A second client with no shared in-process state sees the same data.
	c2 := facadeClient(t, f)
	info, err := c2.SSHInfo(ctx, "k1")
	if err != nil || info.KeyReference != "laptop-ed25519" {
		t.Fatalf("%v %+v", err, info)
	}
	vm, _ := c2.Get(ctx, "k1")
	if vm.Labels["team"] != "x" {
		t.Fatalf("%+v", vm)
	}
}

// A lost or failed Create response must still let the caller find the VM.
func TestAmbiguousCreateIsReconcilableOverRPC(t *testing.T) {
	be := vmlibtest.New(1, "base")
	be.StartUncertainErr = errors.New("response lost after InstanceStart")
	svc, _ := vmlib.NewService(be)
	c := facadeClient(t, serveFacade(t, svc))
	ctx := context.Background()
	vm, err := c.Create(ctx, vmlib.CreateRequest{ImageID: "base", Profile: "small"}) // no ID supplied
	if err == nil {
		t.Fatal("expected error")
	}
	if vm.ID == "" {
		t.Fatal("client must know the VM ID after an ambiguous create (client-allocated ID)")
	}
	got, gerr := c.Get(ctx, vm.ID)
	if gerr != nil || got.State != vmlib.StateRunning {
		t.Fatalf("orphan not reconcilable: %v %+v", gerr, got)
	}
	if res, derr := c.Delete(ctx, vm.ID); derr != nil || !res.Deleted {
		t.Fatalf("%v %+v", derr, res)
	}
}

func TestRetainedDeleteKeepsStructuredResultOverRPC(t *testing.T) {
	be := vmlibtest.New(1, "base")
	be.AutoExit = false
	svc, _ := vmlib.NewService(be)
	c := facadeClient(t, serveFacade(t, svc, remote.WithOperationTimeout(100*time.Millisecond)))
	if _, err := c.Create(context.Background(), vmlib.CreateRequest{ID: "d1", ImageID: "base", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.Delete(ctx, "d1")
	if !errors.Is(err, vmlib.ErrRetained) || !res.Retained || res.Deleted || res.Detail == "" {
		t.Fatalf("retained result lost across RPC: %v %+v", err, res)
	}
	vm, gerr := c.Get(context.Background(), "d1")
	if gerr != nil || !vm.Retained || vm.RetainedReason == "" {
		t.Fatalf("retained state not queryable: %v %+v", gerr, vm)
	}
}

// A remote client waits for readiness with ITS prober (it cannot dial the
// host-private guest); the default must not be mistaken for authenticated SSH.
func TestFacadeClientWaitSSHReadyUsesClientSideProber(t *testing.T) {
	svc, _ := vmlib.NewService(vmlibtest.New(1, "base"))
	f := serveFacade(t, svc)
	calls := 0
	cfg, _ := remote.ClientTLSFromPEM(f.ca.CertPEM, f.client.CertPEM, f.client.KeyPEM, host)
	c, err := remote.DialFacade(f.addr, cfg,
		remote.WithSSHProber(func(_ context.Context, i vmlib.SSHInfo) error {
			calls++
			if calls < 3 || i.ProxyJump() == "" {
				return errors.New("not yet")
			}
			return nil
		}), remote.WithPollInterval(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Create(ctx, vmlib.CreateRequest{ID: "w1", ImageID: "base", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	info, err := c.WaitSSHReady(ctx, "w1")
	if err != nil || !info.Ready || calls != 3 {
		t.Fatalf("%v %+v calls=%d", err, info, calls)
	}
	if _, err := c.Stop(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitSSHReady(ctx, "w1"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("readiness of a stopped VM: %v", err)
	}
}
