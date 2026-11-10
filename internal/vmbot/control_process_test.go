package vmbot

import (
	"context"
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
	if mode == "recover" {
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
	t.Fatal("unknown child mode")
}
func TestControlRealKillRecovery(t *testing.T) {
	for _, mode := range []string{"enqueue-gap", "create-effect", "delete-intent", "delete-effect"} {
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
			if out, e := child("recover").CombinedOutput(); e != nil {
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
			} else if len(st.Snapshot().VMs) != 1 {
				t.Fatal("lost VM")
			}
		})
	}
}
