package vmbot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// A deliberately separate, file-backed remote-resource stand-in. It survives
// the controller process and its SQLite checkpoints independently.
type diskLC struct {
	vmlib.Lifecycle
	root, mode string
}

func (d *diskLC) path(id string) string { return filepath.Join(d.root, "remote", id) }
func (d *diskLC) marker(name string) {
	if err := os.WriteFile(filepath.Join(d.root, name), []byte("ready"), 0600); err != nil {
		panic(err)
	}
}
func (d *diskLC) Create(_ context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
	f, err := os.OpenFile(d.path(r.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return vmlib.VM{}, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return vmlib.VM{}, err
	}
	f.Close()
	_ = syncDir(filepath.Dir(d.path(r.ID)))
	f, err = os.OpenFile(filepath.Join(d.root, "creates"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return vmlib.VM{}, err
	}
	_, err = f.WriteString(r.ID + "\n")
	_ = f.Sync()
	f.Close()
	if err != nil {
		return vmlib.VM{}, err
	}
	if d.mode == "create-effect" || d.mode == "create-delete-intent" {
		d.marker("effect")
		select {}
	}
	return vmlib.VM{ID: r.ID, State: vmlib.StateRunning}, nil
}
func (d *diskLC) Get(_ context.Context, id string) (vmlib.VM, error) {
	_, err := os.Stat(d.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return vmlib.VM{}, vmlib.ErrNotFound
	}
	return vmlib.VM{ID: id, State: vmlib.StateRunning}, err
}
func (d *diskLC) Delete(_ context.Context, id string) (vmlib.DeleteResult, error) {
	err := os.Remove(d.path(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return vmlib.DeleteResult{}, err
	}
	_ = syncDir(filepath.Dir(d.path(id)))
	if d.mode == "delete-effect" {
		d.marker("effect")
		select {}
	}
	return vmlib.DeleteResult{Deleted: true}, nil
}
func (d *diskLC) SSHInfo(context.Context, string) (vmlib.SSHInfo, error) {
	return vmlib.SSHInfo{Host: "127.0.0.1", User: "root"}, nil
}

type diskTerms struct{ lc *diskLC }

func (d diskTerms) Start(context.Context, TerminalTarget) error { d.lc.marker("granted"); return nil }
func (d diskTerms) Revoke(string) {
	if d.lc.mode == "delete-intent" || d.lc.mode == "create-delete-intent" {
		d.lc.marker("intent")
		select {}
	}
}
func (d diskTerms) Close() {}

func waitMarker(ctx context.Context, path string) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// This test is also an actual subprocess entrypoint. No production VM, network,
// token, ttyd or helper is involved; the parent sends real SIGKILL.
func TestOrchestrationProcessWorker(t *testing.T) {
	root := os.Getenv("VMBOT_TEST_CHILD_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	mode := os.Getenv("VMBOT_TEST_CHILD_MODE")
	st, err := OpenStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lc := &diskLC{root: root, mode: mode}
	if mode == "delete-intent" {
		// Crash with domain intent durable but no workflow submission at all.
		// This covers the JSON/outbox gap independently of queue execution.
		if err := st.Update(func(s *State) { s.update("bot-existing", func(r *VMRecord) { r.Phase = PhaseDeleting; r.Seq = 1 }) }); err != nil {
			t.Fatal(err)
		}
		lc.marker("intent")
		select {}
	}
	c := NewController(lc, st, diskTerms{lc}, ControllerConfig{ImageID: "base", Profile: "small", OpTimeout: 3 * time.Second, QueryTimeout: time.Second})
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	switch mode {
	case "create-effect":
		_, err = c.Create(context.Background(), "alpha")
	case "delete-intent", "delete-effect":
		_, err = c.Delete(context.Background(), Ref{Sel: "alpha"})
	case "create-delete-intent":
		go c.Create(context.Background(), "alpha")
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err = waitMarker(ctx, filepath.Join(root, "effect")); err != nil {
			t.Fatal(err)
		}
		_, err = c.Delete(context.Background(), Ref{Sel: "alpha"})
	case "recover":
		err = c.Recover(context.Background())
	default:
		t.Fatal("unknown child mode")
	}
	if err != nil {
		t.Fatal(err)
	}
}

func childCmd(t *testing.T, ctx context.Context, root, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOrchestrationProcessWorker$", "-test.timeout=12s")
	cmd.Env = append(os.Environ(), "VMBOT_TEST_CHILD_ROOT="+root, "VMBOT_TEST_CHILD_MODE="+mode)
	return cmd
}

func TestOrchestrationRealKillRecovery(t *testing.T) {
	for _, mode := range []string{"create-effect", "delete-intent", "delete-effect", "create-delete-intent"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "remote"), 0700); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "delete-") {
				st, err := OpenStore(filepath.Join(root, "state"))
				if err != nil {
					t.Fatal(err)
				}
				rec := VMRecord{ID: "bot-existing", Token: testTok(123), Phase: PhaseActive, Generation: 1, Label: "alpha"}
				err = st.Update(func(s *State) { s.Generation = 1; s.VMs = []VMRecord{rec} })
				if err != nil {
					t.Fatal(err)
				}
				st.Close()
				if err = os.WriteFile(filepath.Join(root, "remote", rec.ID), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := childCmd(t, ctx, root, mode)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill() }()
			marker := "effect"
			if mode == "delete-intent" || mode == "create-delete-intent" {
				marker = "intent"
			}
			if err := waitMarker(ctx, filepath.Join(root, marker)); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatal(err)
			}
			// The second process cannot acquire the lifetime lock while the first lives.
			if st, err := OpenStore(filepath.Join(root, "state")); !errors.Is(err, ErrLocked) {
				if st != nil {
					st.Close()
				}
				t.Fatalf("second instance: %v", err)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("expected actual killed process")
			}
			rctx, rcancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer rcancel()
			if out, err := childCmd(t, rctx, root, "recover").CombinedOutput(); err != nil {
				t.Fatalf("recovery: %v\n%s", err, out)
			}
			st, err := OpenStore(filepath.Join(root, "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			resources, err := os.ReadDir(filepath.Join(root, "remote"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "create-effect" {
				if len(resources) != 1 || len(st.Snapshot().VMs) != 1 || st.Snapshot().VMs[0].Phase != PhaseActive {
					t.Fatal("create recovery lost or duplicated ownership")
				}
				b, err := os.ReadFile(filepath.Join(root, "creates"))
				if err != nil || len(strings.Fields(string(b))) != 1 {
					t.Fatal("external Create must not repeat after an existing effect")
				}
				if _, err := os.Stat(filepath.Join(root, "granted")); err != nil {
					t.Fatal("active terminal projection not rebuilt")
				}
			} else {
				if len(resources) != 0 || len(st.Snapshot().VMs) != 0 {
					t.Fatal("durable delete must converge to absence")
				}
				if _, err := os.Stat(filepath.Join(root, "granted")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("delete recovery must never reauthorize a terminal")
				}
			}
		})
	}
}
