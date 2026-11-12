package main

import (
	"context"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/controlapi"
	"github.com/petruekhin/vibe-firecracker/internal/localcfg"
	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
)

func TestRequiresConfiguration(t *testing.T) {
	if e := run(context.Background(), "", openLocalBackend); e == nil {
		t.Fatal("missing configuration accepted")
	}
}

type stubCreds struct{ sshaccess.Credentials }

func TestIdleConnectionCounterFailsClosedWithoutTrust(t *testing.T) {
	var cfg controlapi.Config
	cfg.Backend = &localcfg.JSONConfig{}
	if idleConnectionCounter(cfg, nil) != nil {
		t.Fatal("counter without credentials")
	}
	cfg.Backend.PersonalSSH = &localcfg.PersonalSSHConfig{Enabled: true, StoreDir: "/s", MasterKeyFile: "/m"}
	if idleConnectionCounter(cfg, stubCreds{}) != nil {
		t.Fatal("counter without strict known-hosts trust")
	}
	cfg.Backend.PersonalSSH.ShutdownKnownHostsFile = "/k"
	if idleConnectionCounter(cfg, stubCreds{}) == nil {
		t.Fatal("trusted configuration should enable the counter")
	}
}
