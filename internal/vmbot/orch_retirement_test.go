package vmbot

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
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

func interruptedLegacyBody(_ dbos.Context, _ OpInput) (OpResult, error) {
	interruptedLegacyRuns.Add(1)
	if err := os.WriteFile(os.Getenv("VMBOT_TEST_RETIREMENT_STARTED"), []byte("started"), 0o600); err != nil {
		return OpResult{}, err
	}
	// Never return, even on cancellation: only an external kill may interrupt
	// this previous-build execution, without checkpointing an empty success.
	select {}
}

// Invoked only in a disposable subprocess, using the current application
// version so the controller must recover (and retire) this workflow.
func TestOrchestrationRetirementCrashFixture(t *testing.T) {
	dir := os.Getenv("VMBOT_TEST_RETIREMENT_STATE")
	if dir == "" {
		return // helper is inert in the ordinary suite
	}
	orch := filepath.Join(dir, orchSubdir)
	if err := os.MkdirAll(orch, 0o700); err != nil {
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
	if _, err = dbos.RunWorkflow(old, interruptedLegacyBody, OpInput{Kind: "delete", VMID: legacyVM, Seq: 1}, dbos.WithWorkflowID("delete-"+legacyVM+"-1")); err != nil {
		t.Fatal(err)
	}
	select {} // parent waits for the workflow's marker, then kills this process
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
	t.Cleanup(func() { _ = st.Close() })
	const id = "delete-" + legacyVM + "-1"
	marker := filepath.Join(dir, "retirement-started")
	// Follow the real-kill fixture used by owner_version_test.go. Shutdown is
	// cooperative and can race a successful checkpoint; it is not a crash.
	child := exec.Command(os.Args[0], "-test.run=^TestOrchestrationRetirementCrashFixture$", "-test.timeout=30s")
	child.Env = append(os.Environ(), "VMBOT_TEST_RETIREMENT_STATE="+dir, "VMBOT_TEST_RETIREMENT_STARTED="+marker)
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = waitMarker(ctx, marker); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		waited = true
		t.Fatalf("previous-build fixture did not start: %v\n%s", err, output.String())
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = child.Wait()
	waited = true
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ProcessState.String() != "signal: killed" {
		t.Fatalf("expected externally killed fixture, got %v\n%s", err, output.String())
	}

	// Inspect the durable state after the kill and before launching recovery.
	// No executor is running, so a SUCCESS here cannot masquerade as recovery.
	db, err := sql.Open("sqlite", filepath.Join(dir, orchSubdir, orchDBName))
	if err != nil {
		t.Fatal(err)
	}
	var status, version, name string
	err = db.QueryRow("SELECT status, application_version, name FROM workflow_status WHERE workflow_uuid = ?", id).Scan(&status, &version, &name)
	closeErr := db.Close()
	if err != nil || closeErr != nil || status != string(dbos.WorkflowStatusPending) || version != orchAppVersion || name != orchWorkflow {
		t.Fatalf("crashed fixture must leave current-version legacy workflow PENDING: status=%q version=%q name=%q query=%v close=%v", status, version, name, err, closeErr)
	}

	base, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	remoteVM(t, base)
	terms := &fakeTerms{}
	c := NewController(base, st, terms, ControllerConfig{Profile: "small", OpTimeout: 2 * time.Second})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 2})
	before := st.Snapshot()
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	h, err := dbos.RetrieveWorkflow[OpResult](c.engine().ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.GetResult(dbos.WithHandleTimeout(5 * time.Second))
	if err != nil || r.Code != codeFailed || r.Msg != "legacy workflow retired by control migration" {
		t.Fatalf("interrupted legacy workflow must be retired by the stub: %+v %v", r, err)
	}
	if n := interruptedLegacyRuns.Load(); n != 0 {
		t.Fatalf("the previous body ran again in the recovering process (%d)", n)
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
