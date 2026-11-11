package vmbot

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

var interruptedLegacyRuns atomic.Int32

func interruptedLegacyBody(ctx dbos.Context, _ OpInput) (OpResult, error) {
	interruptedLegacyRuns.Add(1)
	<-ctx.Done()
	return OpResult{}, nil
}

// A vmbot-op execution left PENDING by a previous build of the CURRENT
// application version (the same one this build runs) is recovered by the
// retirement stub: it completes with the retired result and has no effect on
// the VM, the store or the terminals. Removing the registration would leave such
// an execution unrecoverable.
func TestOrchestrationRecoversInterruptedLegacyWorkflowAsRetired(t *testing.T) {
	interruptedLegacyRuns.Store(0)
	dir := filepath.Join(t.TempDir(), "state")
	st, err := OpenControlStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	orch := filepath.Join(dir, orchSubdir)
	if err = os.MkdirAll(orch, 0o700); err != nil {
		t.Fatal(err)
	}
	old, err := dbos.NewContext(context.Background(), dbos.Config{AppName: orchAppName, DatabaseURL: "sqlite:" + filepath.Join(orch, orchDBName), ExecutorID: orchExecutorID, ApplicationVersion: orchAppVersion})
	if err != nil {
		t.Fatal(err)
	}
	dbos.RegisterWorkflow(old, interruptedLegacyBody, dbos.WithWorkflowName(orchWorkflow))
	if err = dbos.Launch(old); err != nil {
		t.Fatal(err)
	}
	const id = "delete-" + legacyVM + "-1"
	if _, err = dbos.RunWorkflow(old, interruptedLegacyBody, OpInput{Kind: "delete", VMID: legacyVM, Seq: 1}, dbos.WithWorkflowID(id)); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(3 * time.Second); interruptedLegacyRuns.Load() == 0 && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
	}
	if interruptedLegacyRuns.Load() != 1 {
		t.Fatal("previous-build fixture did not start")
	}
	_ = dbos.Shutdown(old, time.Second) // crash: the execution stays PENDING

	base, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	remoteVM(t, base)
	terms := &fakeTerms{}
	c := NewController(base, st, terms, ControllerConfig{Profile: "small", OpTimeout: 2 * time.Second})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 2})
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(); st.Close() }()

	before := st.Snapshot()
	h, err := dbos.RetrieveWorkflow[OpResult](c.engine().ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.GetResult(dbos.WithHandleTimeout(5 * time.Second))
	if err != nil || r.Code != codeFailed || r.Msg != "legacy workflow retired by control migration" {
		t.Fatalf("interrupted legacy workflow must be retired by the stub: %+v %v", r, err)
	}
	if n := interruptedLegacyRuns.Load(); n != 1 {
		t.Fatalf("the previous body ran again (%d)", n)
	}
	if vm, e := base.Get(context.Background(), legacyVM); e != nil || vm.State != vmlib.StateRunning {
		t.Fatalf("retired workflow had an effect: %+v %v", vm, e)
	}
	if !reflect.DeepEqual(st.Snapshot(), before) {
		t.Fatalf("retired workflow changed the store:\n%+v\n%+v", before, st.Snapshot())
	}
	if len(terms.started) != 0 || len(terms.revoked) != 0 {
		t.Fatal("retired workflow touched terminals")
	}
}
