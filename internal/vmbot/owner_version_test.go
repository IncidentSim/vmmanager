package vmbot

import (
	"context"
	"os"
	"os/exec"
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
	if marker := os.Getenv("VMBOT_TEST_LEGACY_STARTED"); marker != "" {
		if err := os.WriteFile(marker, nil, 0600); err != nil {
			return OpResult{}, err
		}
	}
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
	marker := filepath.Join(dir, "legacy-started")
	child := exec.Command(os.Args[0], "-test.run=^TestOwnerLegacyCrashFixture$")
	child.Env = append(os.Environ(), "VMBOT_TEST_LEGACY_STARTED="+marker, "VMBOT_TEST_LEGACY_STATE="+dir)
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
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("legacy fixture did not start")
		}
	}
	// A cooperative Shutdown lets legacyBody return success and is not a crash.
	// Kill the isolated executor so its durable workflow really remains PENDING.
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	waited = true

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
	if n := legacyBodyRuns.Load(); n != 0 {
		t.Fatalf("legacy workflow was re-executed by the new build (%d runs)", n)
	}
	ws, err := dbos.ListWorkflows(c.engine().ctx, dbos.WithFilterWorkflowIDs("control-op-legacy"))
	if err != nil || len(ws) != 1 || ws[0].Status != dbos.WorkflowStatusPending || ws[0].ApplicationVersion != "vmbot-orch-1" {
		t.Fatalf("legacy workflow must stay PENDING under its own version: %v %+v", err, ws)
	}
}

// Invoked only as a disposable subprocess; never owns the parent's Store.
func TestOwnerLegacyCrashFixture(t *testing.T) {
	dir := os.Getenv("VMBOT_TEST_LEGACY_STATE")
	if dir == "" {
		t.Skip("isolated legacy crash fixture")
	}
	orch := filepath.Join(dir, orchSubdir)
	if err := os.MkdirAll(orch, 0o700); err != nil {
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
	select {} // parent kills this process after legacyBody publishes its marker
}
