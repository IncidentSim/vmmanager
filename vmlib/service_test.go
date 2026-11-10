package vmlib_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"example.com/firecracker-postgres-builder/vmlib"
	"example.com/firecracker-postgres-builder/vmlib/vmlibtest"
)

func newSvc(t *testing.T, max int) (*vmlib.Service, *vmlibtest.Backend) {
	t.Helper()
	b := vmlibtest.New(max, "base")
	s, err := vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	return s, b
}

func create(t *testing.T, s vmlib.Lifecycle, id string) vmlib.VM {
	t.Helper()
	vm, err := s.Create(context.Background(), vmlib.CreateRequest{ID: id, ImageID: "base", Profile: "small"})
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return vm
}

func TestCreateRunsAndSSHInfoHasProxyJump(t *testing.T) {
	s, _ := newSvc(t, 2)
	vm := create(t, s, "a1")
	if vm.State != vmlib.StateRunning {
		t.Fatalf("state %s", vm.State)
	}
	info, err := s.SSHInfo(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Host != "172.16.1.2" || info.Port != 22 || info.User != "root" || info.AuthMode != vmlib.AuthImagePreinstalled {
		t.Fatalf("%+v", info)
	}
	if info.HostKeyPolicy != vmlib.HostKeyAcceptNewPerVM {
		t.Fatalf("policy %s", info.HostKeyPolicy)
	}
	if got := info.ProxyJump(); got != "jump@bastion.example.test" {
		t.Fatalf("proxyjump %q", got)
	}
	args := info.SSHArgs("/tmp/kh", "")
	if args[0] != "-J" || args[len(args)-1] != "root@172.16.1.2" {
		t.Fatalf("%v", args)
	}
	if v := info.AnsibleVars(""); v["ansible_ssh_common_args"] != "-o ProxyJump=jump@bastion.example.test" {
		t.Fatalf("%v", v)
	}
}

func TestSSHInfoNeverContainsPrivateKeyMaterial(t *testing.T) {
	s, _ := newSvc(t, 1)
	_, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "k1", ImageID: "base", Profile: "small", SSHKeyReference: "laptop-ed25519"})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := s.SSHInfo(context.Background(), "k1")
	if info.KeyReference != "laptop-ed25519" {
		t.Fatalf("key reference %q", info.KeyReference)
	}
}

func TestValidationRejectsBadInput(t *testing.T) {
	s, _ := newSvc(t, 2)
	for _, req := range []vmlib.CreateRequest{
		{ID: "../x", ImageID: "base", Profile: "small"},
		{ID: "ok", ImageID: "/etc/passwd", Profile: "small"},
		{ID: "ok", ImageID: "base", Profile: "Bad Profile"},
		{ID: "ok", ImageID: "base", Profile: "small", SSHPublicKey: "ssh-ed25519 AAA\nrm -rf /"},
		{ID: "ok", ImageID: "base", Profile: "small", SSHPublicKey: "not a key"},
	} {
		if _, err := s.Create(context.Background(), req); !errors.Is(err, vmlib.ErrInvalidArgument) {
			t.Errorf("%+v: %v", req, err)
		}
	}
	if _, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "ok", ImageID: "missing", Profile: "small"}); !errors.Is(err, vmlib.ErrNotFound) {
		t.Errorf("missing image: %v", err)
	}
	if _, err := s.Get(context.Background(), "nope"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Errorf("get: %v", err)
	}
}

func TestDuplicateIDRejected(t *testing.T) {
	s, _ := newSvc(t, 2)
	create(t, s, "dup")
	if _, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "dup", ImageID: "base", Profile: "small"}); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("%v", err)
	}
}

func TestGeneratedIDsAreUniqueAndValid(t *testing.T) {
	s, _ := newSvc(t, 3)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		vm, err := s.Create(context.Background(), vmlib.CreateRequest{ImageID: "base", Profile: "small"})
		if err != nil {
			t.Fatal(err)
		}
		if vmlib.ValidateID("vm", vm.ID) != nil || seen[vm.ID] {
			t.Fatalf("bad id %q", vm.ID)
		}
		seen[vm.ID] = true
	}
}

func TestParallelCreatesRespectCapAndUniqueIPs(t *testing.T) {
	const max = 3
	s, b := newSvc(t, max)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, exhausted := 0, 0
	ips := map[string]bool{}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			vm, err := s.Create(context.Background(), vmlib.CreateRequest{ID: fmt.Sprintf("p%d", i), ImageID: "base", Profile: "small"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
				info, _ := s.SSHInfo(context.Background(), vm.ID)
				if ips[info.Host] {
					t.Errorf("duplicate guest IP %s", info.Host)
				}
				ips[info.Host] = true
			case errors.Is(err, vmlib.ErrResourceExhausted):
				exhausted++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok != max || exhausted != 12-max || b.Count() != max {
		t.Fatalf("ok=%d exhausted=%d leases=%d", ok, exhausted, b.Count())
	}
}

func TestFailedCreateFreesLeases(t *testing.T) {
	s, b := newSvc(t, 1)
	b.StartErr = errors.New("boom")
	if _, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "f1", ImageID: "base", Profile: "small"}); err == nil {
		t.Fatal("expected error")
	}
	if b.Count() != 0 {
		t.Fatal("lease leaked after definite pre-start failure")
	}
	b.StartErr = nil
	create(t, s, "f2")
}

func TestUncertainStartKeepsVMAndDisk(t *testing.T) {
	s, b := newSvc(t, 1)
	b.StartUncertainErr = errors.New("response lost")
	vm, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "u1", ImageID: "base", Profile: "small"})
	if err == nil {
		t.Fatal("expected error")
	}
	_ = vm
	if b.Count() != 1 || b.Released != 0 {
		t.Fatal("a possibly running VM must keep its lease/disk")
	}
	got, err := s.Get(context.Background(), "u1")
	if err != nil || got.State != vmlib.StateRunning {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestCreateContextCancelDoesNotKillRunningVM(t *testing.T) {
	s, b := newSvc(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	create := func() { _, _ = s.Create(ctx, vmlib.CreateRequest{ID: "c1", ImageID: "base", Profile: "small"}) }
	create()
	cancel() // the RPC context ends after Create returned
	time.Sleep(20 * time.Millisecond)
	vm, err := s.Get(context.Background(), "c1")
	if err != nil || vm.State != vmlib.StateRunning || b.Released != 0 {
		t.Fatalf("running VM was affected by context end: %v %+v", err, vm)
	}
}

func TestCancelledBeforeCreateLeavesNothing(t *testing.T) {
	s, b := newSvc(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Create(ctx, vmlib.CreateRequest{ID: "x1", ImageID: "base", Profile: "small"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if b.Count() != 0 {
		t.Fatal("leak")
	}
}

func TestCancelDuringStartRollsBackBeforeInstanceStart(t *testing.T) {
	s, b := newSvc(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	b.StartErr = context.Canceled // backend saw cancellation before launching anything
	cancel()
	_, err := s.Create(ctx, vmlib.CreateRequest{ID: "r1", ImageID: "base", Profile: "small"})
	if err == nil || b.Count() != 0 {
		t.Fatalf("%v leases=%d", err, b.Count())
	}
}

func TestStopIsIdempotentAndConcurrentSafe(t *testing.T) {
	s, b := newSvc(t, 1)
	create(t, s, "s1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vm, err := s.Stop(context.Background(), "s1")
			if err != nil || vm.State != vmlib.StateStopped {
				t.Errorf("%v %+v", err, vm)
			}
		}()
	}
	wg.Wait()
	if _, err := s.Stop(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if b.Released != 0 {
		t.Fatal("stop must keep the disk")
	}
}

func TestStopWithoutReceiptIsNotStoppedAndRetryable(t *testing.T) {
	s, b := newSvc(t, 1)
	b.AutoExit = false
	create(t, s, "h1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	vm, err := s.Stop(ctx, "h1")
	if err == nil || vm.State != vmlib.StateStopping {
		t.Fatalf("%v %+v", err, vm)
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer ccancel()
	if _, err := s.Commit(cctx, "h1", "newimg"); err == nil {
		t.Fatal("commit must not publish without an exit receipt")
	}
	if b.Published != 0 {
		t.Fatal("published without receipt")
	}
	b.ExitVM("h1", true) // receipt finally appears
	if vm, err := s.Stop(context.Background(), "h1"); err != nil || vm.State != vmlib.StateStopped {
		t.Fatalf("%v %+v", err, vm)
	}
}

func TestCommitPublishesNewImageAndKeepsBaseImmutable(t *testing.T) {
	s, b := newSvc(t, 3) // m1 keeps its lease until deleted
	create(t, s, "m1")
	img, err := s.Commit(context.Background(), "m1", "golden")
	if err != nil || img.ID != "golden" || img.Base {
		t.Fatalf("%v %+v", err, img)
	}
	if _, err := s.Commit(context.Background(), "m1", "base"); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("overwriting base must fail: %v", err)
	}
	if _, err := s.Commit(context.Background(), "m1", "golden"); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("overwriting committed image must fail: %v", err)
	}
	imgs, _ := s.ListImages(context.Background())
	if len(imgs) != 2 {
		t.Fatalf("%+v", imgs)
	}
	base, _ := b.GetImage(context.Background(), "base")
	if base.SHA256 != "base-base" {
		t.Fatal("base changed")
	}
	// Independent VMs from the committed image.
	for _, id := range []string{"i1", "i2"} {
		if _, err := s.Create(context.Background(), vmlib.CreateRequest{ID: id, ImageID: "golden", Profile: "small"}); err != nil {
			t.Fatal(err)
		}
	}
	i1, _ := s.SSHInfo(context.Background(), "i1")
	i2, _ := s.SSHInfo(context.Background(), "i2")
	if i1.Host == i2.Host {
		t.Fatal("independent VMs share an address")
	}
}

func TestUncleanExitBlocksCommitAndRetainsDisk(t *testing.T) {
	s, b := newSvc(t, 1)
	b.UncleanExit = true
	create(t, s, "bad")
	if _, err := s.Commit(context.Background(), "bad", "nope"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("commit after unclean exit must fail: %v", err)
	}
	if b.Published != 0 {
		t.Fatal("published after unclean exit")
	}
	vm, _ := s.Get(context.Background(), "bad")
	if vm.State != vmlib.StateFailed || b.Count() != 1 {
		t.Fatalf("%+v leases=%d", vm, b.Count())
	}
	// The exit is receipted, so only an explicit Delete may free the disk.
	if res, err := s.Delete(context.Background(), "bad"); err != nil || !res.Deleted {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestDeleteRunningVMWithoutReceiptIsRetainedExplicitly(t *testing.T) {
	s, b := newSvc(t, 1)
	b.AutoExit = false
	create(t, s, "d1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	res, err := s.Delete(ctx, "d1")
	if !errors.Is(err, vmlib.ErrRetained) || res.Deleted || !res.Retained {
		t.Fatalf("%v %+v", err, res)
	}
	vm, _ := s.Get(context.Background(), "d1")
	if !vm.Retained || vm.RetainedReason == "" {
		t.Fatalf("%+v", vm)
	}
	if b.Count() != 1 {
		t.Fatal("lease freed on uncertain process state")
	}
	b.ExitVM("d1", true)
	res, err = s.Delete(context.Background(), "d1")
	if err != nil || !res.Deleted {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestDeleteIsIdempotentAndConcurrentSafe(t *testing.T) {
	s, b := newSvc(t, 1)
	create(t, s, "x")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res, err := s.Delete(context.Background(), "x"); err != nil || !res.Deleted {
				t.Errorf("%v %+v", err, res)
			}
		}()
	}
	wg.Wait()
	if b.Count() != 0 || b.Released != 1 {
		t.Fatalf("leases=%d released=%d", b.Count(), b.Released)
	}
	if res, err := s.Delete(context.Background(), "x"); err != nil || !res.Deleted {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := s.Get(context.Background(), "x"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestRehydratesFromBackendAfterServiceRestart(t *testing.T) {
	b := vmlibtest.New(2, "base")
	s1, _ := vmlib.NewService(b)
	create(t, s1, "r1")
	s2, _ := vmlib.NewService(b) // new process, same host state
	vm, err := s2.Get(context.Background(), "r1")
	if err != nil || vm.State != vmlib.StateRunning {
		t.Fatalf("%v %+v", err, vm)
	}
	list, _ := s2.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("%+v", list)
	}
	if _, err := s2.Stop(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
}

func TestWaitSSHReadyUsesProber(t *testing.T) {
	b := vmlibtest.New(1, "base")
	calls := 0
	s, _ := vmlib.NewService(b, vmlib.WithSSHProber(func(ctx context.Context, i vmlib.SSHInfo) error {
		calls++
		if calls < 3 {
			return errors.New("not yet")
		}
		return nil
	}), vmlib.WithPollInterval(time.Millisecond))
	create(t, s, "w1")
	info, err := s.WaitSSHReady(context.Background(), "w1")
	if err != nil || !info.Ready || calls != 3 {
		t.Fatalf("%v %+v calls=%d", err, info, calls)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	s2, _ := vmlib.NewService(b, vmlib.WithSSHProber(func(context.Context, vmlib.SSHInfo) error { return errors.New("never") }), vmlib.WithPollInterval(time.Millisecond))
	if _, err := s2.WaitSSHReady(ctx, "w1"); err == nil {
		t.Fatal("expected timeout")
	}
}

func TestPublicKeyProvisioningIsReportedNotReturned(t *testing.T) {
	s, b := newSvc(t, 1)
	c, _ := b.Capabilities(context.Background())
	c.SSHAuthMode = vmlib.AuthProvisionPublicKey
	b.SetCapabilities(c)
	_, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "pk", ImageID: "base", Profile: "small",
		SSHPublicKey: "ssh-ed25519 AAAAC3Nza test@laptop"})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := s.SSHInfo(context.Background(), "pk")
	if !info.PublicKeyProvisioned || info.AuthMode != vmlib.AuthProvisionPublicKey {
		t.Fatalf("%+v", info)
	}
}
