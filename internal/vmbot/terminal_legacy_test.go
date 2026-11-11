package vmbot

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

type missingTerminalCredentials struct{ calls int }

func (c *missingTerminalCredentials) TerminalKeyFile(string, string) (string, func(), error) {
	c.calls++
	return "", nil, sshaccess.ErrNotFound
}

func TestTerminalExplicitLegacyUsesConfiguredKeyWithPersonalAccessEnabled(t *testing.T) {
	c := &missingTerminalCredentials{}
	e := newTerm(t, func(cfg *ManagerConfig) { cfg.Credentials = c })
	target := target("legacy-running", tokA)
	target.AuthMode = vmlib.AuthImagePreinstalled
	if err := e.m.Start(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(e.m.sockPath(target.VMID) + ".argv")
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	found := false
	for i, arg := range args {
		if arg == "-i" && i+1 < len(args) && args[i+1] == e.m.cfg.SSHKeyFile {
			found = true
		}
	}
	if !found {
		t.Fatal("legacy ttyd did not receive configured legacy identity")
	}
	if c.calls != 0 {
		t.Fatal("legacy VM consulted personal credential store")
	}
}

func TestTerminalManagedMissingCredentialNeverUsesLegacyKey(t *testing.T) {
	for _, mode := range []string{vmlib.AuthProvisionPublicKey, "", "unknown"} {
		t.Run("mode="+mode, func(t *testing.T) {
			c := &missingTerminalCredentials{}
			e := newTerm(t, func(cfg *ManagerConfig) { cfg.Credentials = c })
			target := target("managed-missing", tokA)
			target.AuthMode = mode
			if err := e.m.Start(context.Background(), target); err == nil {
				t.Fatal("missing managed credential used legacy key")
			}
			if c.calls != 1 {
				t.Fatal("did not require dedicated credential")
			}
			if _, err := os.Stat(e.m.sockPath(target.VMID) + ".argv"); !os.IsNotExist(err) {
				t.Fatal("launched runner on missing managed credential", err)
			}
		})
	}
}

func TestTerminalManagedWithoutCredentialProviderFailsClosed(t *testing.T) {
	e := newTerm(t, nil)
	target := target("managed-no-provider", tokA)
	target.AuthMode = vmlib.AuthProvisionPublicKey
	if err := e.m.Start(context.Background(), target); err == nil {
		t.Fatal("managed VM used legacy key with provider disabled")
	}
}
