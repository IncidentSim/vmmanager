package vmbot

import (
	"context"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

type terminalPolicyLifecycle struct {
	vmlib.Lifecycle
	mode string
}

func (l terminalPolicyLifecycle) SSHInfo(context.Context, string) (vmlib.SSHInfo, error) {
	return vmlib.SSHInfo{Host: "172.16.1.2", User: "root", AuthMode: l.mode}, nil
}

// Exercise the controller -> real Manager -> ttyd executable seam. The VM has
// no TTL or request metadata; neither is evidence of legacy SSH authentication.
func TestControllerSelectsTerminalIdentityFromPerVMPolicy(t *testing.T) {
	for _, mode := range []string{vmlib.AuthImagePreinstalled, vmlib.AuthProvisionPublicKey} {
		t.Run(mode, func(t *testing.T) {
			st, err := OpenStore(t.TempDir() + "/state")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			record := VMRecord{ID: "policy-vm", Token: tokA, Phase: PhaseActive, Generation: 1}
			if err := st.UpdateErr(func(s *State) error { s.Generation = 1; s.VMs = []VMRecord{record}; return nil }); err != nil {
				t.Fatal(err)
			}
			credentials := &missingTerminalCredentials{}
			e := newTerm(t, func(cfg *ManagerConfig) { cfg.Credentials = credentials })
			ctl := NewController(terminalPolicyLifecycle{mode: mode}, st, e.m, ControllerConfig{})
			err = ctl.ensureTerminalLocked(context.Background(), record)
			if mode == vmlib.AuthImagePreinstalled {
				if err != nil || credentials.calls != 0 {
					t.Fatalf("legacy VM lost terminal: %v calls=%d", err, credentials.calls)
				}
			} else {
				if err == nil || credentials.calls != 1 {
					t.Fatalf("managed VM silently downgraded: %v calls=%d", err, credentials.calls)
				}
			}
		})
	}
}
