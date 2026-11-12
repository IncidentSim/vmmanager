// vm-network is the host-root TAP/firewall tool of one VM slot, run by
// firecracker-vm-network@.service. It is the Go replacement of the former
// fc-vm-network.sh; see internal/vmnet for the ownership rules.
//
//	vm-network check|up|refresh|down|flush-conntrack N
//
// check is unprivileged. The pool file location is fixed (root-owned).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/petruekhin/vibe-firecracker/internal/vmnet"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	os.Setenv("PATH", "/usr/sbin:/usr/bin:/sbin:/bin")
	os.Setenv("LC_ALL", "C")
	if err := vmnet.Main(ctx, os.Args[1:], vmnet.DefaultPoolConf, vmnet.DefaultStateDir, vmnet.ExecRunner{}, os.Geteuid()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
