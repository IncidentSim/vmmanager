package vmlib_test

import (
	"context"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestPublicSSHTCPProbeUsesInternalTarget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	host, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	info := vmlib.SSHInfo{Host: "203.0.113.10", Port: 22000, InternalTarget: &vmlib.SSHEndpoint{Host: host, Port: port}}
	if err := vmlib.TCPProber(ctx, info); err != nil {
		t.Fatalf("default server probe used public instead of private target: %v", err)
	}
}
