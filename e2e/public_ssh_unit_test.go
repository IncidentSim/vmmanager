package e2e

import (
	"context"
	"github.com/petruekhin/vibe-firecracker/controlclient"
	"strings"
	"testing"
)

func TestPublicSSHEndpointRetainsPortAndNoBastion(t *testing.T) {
	for _, host := range []string{"203.0.113.10", "ssh.example.test"} {
		info := controlclient.SSH{Host: host, Port: 22003, User: "root"}
		g, err := pinGuest(context.Background(), t.TempDir(), "/operator/key", "/operator/known_hosts", info)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Join(g.sshArgs("true"), " ")
		if !strings.Contains(args, "-p 22003") || !strings.Contains(args, "root@"+host) || strings.Contains(args, "-J") || !strings.Contains(args, "StrictHostKeyChecking=yes") {
			t.Fatalf("public SSH argv: %s", args)
		}
		line, ok := knownHostsLine(info.Host, info.Port, "scan ssh-ed25519 AAAA")
		if !ok || line != "["+host+"]:22003 ssh-ed25519 AAAA" {
			t.Fatalf("public key endpoint: %s", line)
		}
	}
}
