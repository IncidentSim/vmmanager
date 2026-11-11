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
)

func TestControlProcessWorker(t *testing.T) {
	root := os.Getenv("CONTROL_TEST_ROOT")
	if root == "" {
		t.Skip("child helper")
	}
	mode := os.Getenv("CONTROL_TEST_MODE")
	st, e := OpenControlStore(filepath.Join(root, "state"), 0)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	lc := &diskLC{root: root, mode: mode}
	c := NewController(lc, st, diskTerms{lc}, ControllerConfig{Profile: "small", OpTimeout: 2 * time.Second})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
	if mode == "enqueue-gap" {
		if _, e = c.AcceptControl(11, "stable-create", ControlRequest{Kind: "create", ImageID: "ubuntu"}); e != nil {
			t.Fatal(e)
		}
		lc.marker("effect")
		select {}
	}
	if e = c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if mode == "stop-before-projection" {
		// The child has completed (its result is checkpointed); die before the
		// owner writes the outcome.
		c.testBeforeProject = func(cmd OwnerCmd) {
			if cmd.Kind == "stop" {
				lc.marker("effect")
				select {}
			}
		}
	}
	if mode == "recover" || mode == "recover-stop" {
		if e = c.RecoverControl(); e != nil {
			t.Fatal(e)
		}
		for _, o := range st.Snapshot().Operations {
			if o.Status == "pending" || o.Status == "running" {
				got := awaitControl(t, c, 11, o.ID)
				if got.Status != "succeeded" {
					t.Fatalf("recovered %+v", got)
				}
			}
		}
		if mode == "recover-stop" {
			// The idle owner that survived the kill must still take a command.
			vm := st.Snapshot().VMs[0]
			stop, e := c.AcceptControl(11, "post-restart-stop", ControlRequest{Kind: "stop", VMID: vm.ID})
			if e != nil {
				t.Fatal(e)
			}
			if got := awaitControl(t, c, 11, stop.ID); got.Status != "succeeded" {
				t.Fatalf("post-restart stop %+v", got)
			}
		}
		return
	}
	create, e := c.AcceptControl(11, "stable-create", ControlRequest{Kind: "create", ImageID: "ubuntu"})
	if e != nil {
		t.Fatal(e)
	}
	if mode == "create-effect" {
		select {}
	}
	awaitControl(t, c, 11, create.ID)
	if mode == "delete-intent" || mode == "delete-effect" {
		if _, e = c.AcceptControl(11, "stable-delete", ControlRequest{Kind: "delete", VMID: create.VMID}); e != nil {
			t.Fatal(e)
		}
		select {}
	}
	switch mode {
	case "idle-owner":
		lc.marker("effect")
		select {}
	case "stop-effect", "stop-before-projection":
		if _, e = c.AcceptControl(11, "stable-stop", ControlRequest{Kind: "stop", VMID: create.VMID}); e != nil {
			t.Fatal(e)
		}
		select {}
	}
	t.Fatal("unknown child mode")
}
func TestControlRealKillRecovery(t *testing.T) {
	for _, mode := range []string{"enqueue-gap", "create-effect", "delete-intent", "delete-effect", "idle-owner", "stop-effect", "stop-before-projection"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if e := os.Mkdir(filepath.Join(root, "remote"), 0700); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			child := func(mode string) *exec.Cmd {
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestControlProcessWorker$", "-test.timeout=10s")
				cmd.Env = append(os.Environ(), "CONTROL_TEST_ROOT="+root, "CONTROL_TEST_MODE="+mode)
				return cmd
			}
			cmd := child(mode)
			if e := cmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer cmd.Process.Kill()
			marker := "effect"
			if mode == "delete-intent" {
				marker = "intent"
			}
			if e := waitMarker(ctx, filepath.Join(root, marker)); e != nil {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal(e)
			}
			if e := cmd.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			if e := cmd.Wait(); e == nil {
				t.Fatal("expected SIGKILL")
			}
			recoverMode := "recover"
			if mode == "idle-owner" {
				recoverMode = "recover-stop"
			}
			if out, e := child(recoverMode).CombinedOutput(); e != nil {
				t.Fatalf("restart: %v %s", e, out)
			}
			st, e := OpenControlStore(filepath.Join(root, "state"), 0)
			if e != nil {
				t.Fatal(e)
			}
			defer st.Close()
			if len(st.Snapshot().Operations) == 0 {
				t.Fatal("lost durable operation")
			}
			for _, o := range st.Snapshot().Operations {
				if o.Status != "succeeded" {
					t.Fatalf("unfinished %+v", o)
				}
			}
			creates, _ := os.ReadFile(filepath.Join(root, "creates"))
			if len(strings.Fields(string(creates))) != 1 {
				t.Fatalf("create repeated: %q", creates)
			}
			if strings.HasPrefix(mode, "delete-") {
				if len(st.Snapshot().VMs) != 0 {
					t.Fatal("delete not settled")
				}
				remote, e := os.ReadDir(filepath.Join(root, "remote"))
				if e != nil || len(remote) != 0 {
					t.Fatalf("durable delete must converge to an absent remote resource: %v %v", remote, e)
				}
				if _, e := os.Stat(filepath.Join(root, "granted")); !errors.Is(e, os.ErrNotExist) {
					t.Fatalf("delete recovery must never grant a terminal: %v", e)
				}
			} else if len(st.Snapshot().VMs) != 1 {
				t.Fatal("lost VM")
			}
			// Raw effect invocations are not exactly-once (DBOS recovery may rerun an
			// effect whose checkpoint was lost); a completed child is never rerun.
			stops, _ := os.ReadFile(filepath.Join(root, "stops"))
			n := len(strings.Fields(string(stops)))
			switch mode {
			case "stop-effect":
				if n < 1 || n > 2 || st.Snapshot().VMs[0].Phase != "stopped" {
					t.Fatalf("stop-effect: raw stops=%d phase=%s", n, st.Snapshot().VMs[0].Phase)
				}
			case "stop-before-projection":
				if n != 1 || st.Snapshot().VMs[0].Phase != "stopped" {
					t.Fatalf("completed child was rerun or outcome lost: stops=%d phase=%s", n, st.Snapshot().VMs[0].Phase)
				}
			case "idle-owner":
				if n != 1 || st.Snapshot().VMs[0].Phase != "stopped" {
					t.Fatalf("idle owner after restart: stops=%d phase=%s", n, st.Snapshot().VMs[0].Phase)
				}
			}
		})
	}
}
