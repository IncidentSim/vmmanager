package vmbot

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

type credentialDeleteFunc func(string) error

func (f credentialDeleteFunc) Delete(id string) error { return f(id) }

func cleanupStore(t *testing.T) *sshaccess.Store {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "master.key")
	if err := sshaccess.GenerateMasterKeyFile(key); err != nil {
		t.Fatal(err)
	}
	st, err := sshaccess.OpenStore(filepath.Join(dir, "credentials"), key)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Configure before Start: recovered effects must see the same cleanup seam.
func startCleanupEnv(t *testing.T, cleanup credentialDeleteFunc, del func(context.Context, string) (vmlib.DeleteResult, error)) *ownerEnv {
	t.Helper()
	e := newOwnerEnv(t)
	e.close()
	e.open(false)
	e.ctl.cfg.Credentials = cleanup
	if del != nil {
		e.ctl.lc = &hookLC{Lifecycle: e.svc, del: del}
	}
	if err := e.ctl.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.ctl.RecoverControl(); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestCredentialCleanupAbsentFacadeDelete(t *testing.T) {
	credentials := cleanupStore(t)
	var calls atomic.Int32
	e := startCleanupEnv(t, func(id string) error {
		calls.Add(1)
		return credentials.Delete(id)
	}, nil)
	created := e.create("create")
	if _, err := credentials.Ensure(created.VMID, ""); err != nil {
		t.Fatal(err)
	}
	// The service's already-absent path does not call backend Release.
	if _, err := e.svc.Delete(context.Background(), created.VMID); err != nil {
		t.Fatal(err)
	}
	deleted := e.accept("delete", ControlRequest{Kind: "delete", VMID: created.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, deleted.ID); got.Status != "succeeded" {
		t.Fatalf("delete: %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("credential deletes = %d", calls.Load())
	}
	if _, err := credentials.Public(created.VMID); !errors.Is(err, sshaccess.ErrNotFound) {
		t.Fatalf("credentials survived: %v", err)
	}
	if _, ok := e.st.Snapshot().ByID(created.VMID); ok {
		t.Fatal("domain record survived successful cleanup")
	}
}

func TestCredentialCleanupFailureRetainsQuotaAndRetriesAfterRestart(t *testing.T) {
	credentials := cleanupStore(t)
	var calls atomic.Int32
	cleanup := credentialDeleteFunc(func(id string) error {
		if calls.Add(1) == 1 {
			return errors.New("credential unlink failed")
		}
		return credentials.Delete(id)
	})
	e := startCleanupEnv(t, cleanup, nil)
	created := e.create("create")
	if _, err := credentials.Ensure(created.VMID, ""); err != nil {
		t.Fatal(err)
	}
	e.create("fill-1")
	e.create("fill-2")
	deleted := e.accept("delete-fails", ControlRequest{Kind: "delete", VMID: created.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, deleted.ID); got.Status != "failed" || got.ErrorCode != "unavailable" {
		t.Fatalf("cleanup failure: %+v", got)
	}
	if _, err := credentials.Public(created.VMID); err != nil {
		t.Fatalf("credentials lost: %v", err)
	}
	if _, err := e.svc.Get(context.Background(), created.VMID); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("backend not deleted: %v", err)
	}
	retained := func() {
		t.Helper()
		if rec, ok := e.st.Snapshot().ByID(created.VMID); !ok || rec.Phase != PhaseDeleting {
			t.Fatalf("record not retained: %+v, %v", rec, ok)
		}
		if _, err := e.ctl.AcceptControl(ownerUser, "over-quota", ControlRequest{Kind: "create", ImageID: "ubuntu"}); !errors.Is(err, ErrCapacity) {
			t.Fatalf("quota released early: %v", err)
		}
	}
	retained()
	e.close()
	e.open(false)
	e.ctl.cfg.Credentials = cleanup
	if err := e.ctl.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.ctl.RecoverControl(); err != nil {
		t.Fatal(err)
	}
	retained()
	if got := e.op(deleted.ID); got.Status != "failed" {
		t.Fatalf("failure not durable: %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("recovery repeated completed failed effect: %d", calls.Load())
	}
	retry := e.accept("delete-retry", ControlRequest{Kind: "delete", VMID: created.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, retry.ID); got.Status != "succeeded" {
		t.Fatalf("retry: %+v", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("cleanup attempts = %d", calls.Load())
	}
	if _, err := credentials.Public(created.VMID); !errors.Is(err, sshaccess.ErrNotFound) {
		t.Fatalf("credentials survived retry: %v", err)
	}
	if _, ok := e.st.Snapshot().ByID(created.VMID); ok {
		t.Fatal("record survived retry")
	}
	e.create("quota-freed")
}

func TestCredentialCleanupNeverRunsForUnprovenDelete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result vmlib.DeleteResult
		err    error
	}{
		{"error", vmlib.DeleteResult{Deleted: true}, context.DeadlineExceeded},
		{"not-deleted", vmlib.DeleteResult{}, nil},
		{"retained-error", vmlib.DeleteResult{Retained: true}, vmlib.ErrRetained},
		{"retained-nil", vmlib.DeleteResult{Retained: true}, nil},
		{"inconsistent-retained", vmlib.DeleteResult{Deleted: true, Retained: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			e := startCleanupEnv(t, func(string) error { calls.Add(1); return nil }, func(context.Context, string) (vmlib.DeleteResult, error) { return tc.result, tc.err })
			created := e.create("create")
			deleted := e.accept("delete", ControlRequest{Kind: "delete", VMID: created.VMID})
			if got := awaitControl(t, e.ctl, ownerUser, deleted.ID); got.Status != "failed" {
				t.Fatalf("unproven delete succeeded: %+v", got)
			}
			if calls.Load() != 0 {
				t.Fatalf("credentials deleted without proof: %d", calls.Load())
			}
			if _, ok := e.st.Snapshot().ByID(created.VMID); !ok {
				t.Fatal("unproven delete removed domain record")
			}
		})
	}
}

// Startup must inject the shared store at construction, before Start recovers
// DBOS workflows (not only before the explicit RecoverControl call).
func TestCredentialCleanupAPIStartupWiring(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "..", "cmd", "vmapi", "main.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wired := false
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typ, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || typ.Sel.Name != "ControllerConfig" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Credentials" {
				continue
			}
			value, ok := kv.Value.(*ast.Ident)
			wired = ok && value.Name == "credentials"
		}
		return true
	})
	if !wired {
		t.Fatal("API ControllerConfig must receive the shared credentials before startup recovery")
	}
}
