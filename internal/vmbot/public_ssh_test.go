package vmbot

import (
	"context"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"testing"
)

type publicTerminalLifecycle struct{ vmlib.Lifecycle }

func (publicTerminalLifecycle) SSHInfo(context.Context, string) (vmlib.SSHInfo, error) {
	return vmlib.SSHInfo{Host: "ssh.example.test", Port: 22003, User: "root", InternalTarget: &vmlib.SSHEndpoint{Host: "172.30.3.2", Port: 22}}, nil
}
func TestPublicSSHTerminalUsesPrivateTarget(t *testing.T) {
	st, err := OpenStore(t.TempDir() + "/state")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec := VMRecord{ID: "public-terminal", Token: tokA, Phase: PhaseActive, Generation: 1}
	if err := st.UpdateErr(func(s *State) error { s.Generation = 1; s.VMs = []VMRecord{rec}; return nil }); err != nil {
		t.Fatal(err)
	}
	term := &fakeTerms{}
	c := NewController(publicTerminalLifecycle{}, st, term, ControllerConfig{})
	if err := c.ensureTerminalLocked(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if len(term.started) != 1 || term.started[0].Host != "172.30.3.2" || term.started[0].ProxyJump != "" {
		t.Fatalf("terminal did not use private guest: %+v", term.started)
	}
	args, err := BuildSSHArgs(ManagerConfig{SSHPath: "/usr/bin/ssh", SSHKeyFile: "/server/key"}, term.started[0], "/server/known_hosts")
	if err != nil {
		t.Fatal(err)
	}
	if args[len(args)-3] != "-p" || args[len(args)-2] != "22" || args[len(args)-1] != "root@172.30.3.2" {
		t.Fatalf("wrong internal SSH argv: %v", args)
	}
}
