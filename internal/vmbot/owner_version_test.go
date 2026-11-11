package vmbot

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// The pinned names are part of the durable contract: an owner may outlive a
// release and is replayed by name. Changing one orphans stored workflows.
func TestOwnerPinnedWorkflowNamesAndVersion(t *testing.T) {
	for got, want := range map[string]string{
		ownerWorkflowName: "vmbot-vm-owner", effectWorkflowName: "vmbot-vm-effect",
		imageWorkflowName: "vmbot-image-delete", orchWorkflow: "vmbot-op",
		orchAppVersion: "vmbot-owner-1", ownerCmdTopic: "cmd",
	} {
		if got != want {
			t.Fatalf("pinned name changed: %q != %q (needs dbos.Patch / a migration plan)", got, want)
		}
	}
	if ownerWorkflowID("bot-x") != "vmbot-owner-bot-x" {
		t.Fatal("owner ID scheme changed")
	}
}

var legacyBodyRuns atomic.Int32

func legacyBody(ctx dbos.Context, in OpInput) (OpResult, error) {
	legacyBodyRuns.Add(1)
	select {
	case <-ctx.Done():
	case <-time.After(time.Minute):
	}
	return OpResult{}, nil
}

// A workflow left PENDING by a pre-owner build (application version
// "vmbot-orch-1") must neither be recovered nor executed by this build: old and
// new executors never own the same VM. It stays untouched in the database.
func TestOwnerLegacyVersionWorkflowIsNotRecovered(t *testing.T) {
	legacyBodyRuns.Store(0) // package-level fixture counter: reset for -count=N
	dir := filepath.Join(t.TempDir(), "state")
	st, err := OpenControlStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	orch := filepath.Join(dir, orchSubdir)
	if err = os.MkdirAll(orch, 0o700); err != nil {
		t.Fatal(err)
	}
	// Act as the previous build: same DB file, executor and workflow name, old version.
	old, err := dbos.NewContext(context.Background(), dbos.Config{AppName: orchAppName, DatabaseURL: "sqlite:" + filepath.Join(orch, orchDBName), ExecutorID: orchExecutorID, ApplicationVersion: "vmbot-orch-1"})
	if err != nil {
		t.Fatal(err)
	}
	dbos.RegisterWorkflow(old, legacyBody, dbos.WithWorkflowName(orchWorkflow))
	if err = dbos.Launch(old); err != nil {
		t.Fatal(err)
	}
	if _, err = dbos.RunWorkflow(old, legacyBody, OpInput{Kind: "control", VMID: "bot-legacy"}, dbos.WithWorkflowID("control-op-legacy")); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(3 * time.Second); legacyBodyRuns.Load() == 0 && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
	}
	if legacyBodyRuns.Load() != 1 {
		t.Fatal("legacy fixture did not start")
	}
	// "Crash": shut down without letting the workflow finish.
	_ = dbos.Shutdown(old, time.Second)

	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	c := NewController(svc, st, &controlTerms{}, ControllerConfig{Profile: "small"})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 2})
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(); st.Close() }()
	if err = c.RecoverControl(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := legacyBodyRuns.Load(); n != 1 {
		t.Fatalf("legacy workflow was re-executed by the new build (%d runs)", n)
	}
	ws, err := dbos.ListWorkflows(c.engine().ctx, dbos.WithFilterWorkflowIDs("control-op-legacy"))
	if err != nil || len(ws) != 1 || ws[0].Status != dbos.WorkflowStatusPending || ws[0].ApplicationVersion != "vmbot-orch-1" {
		t.Fatalf("legacy workflow must stay PENDING under its own version: %v %+v", err, ws)
	}
}
