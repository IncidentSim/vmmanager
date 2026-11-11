// vm-helper is the ROOT side of the privilege boundary (proposed; source only).
// systemd socket-activates one instance per connection (Accept=yes) with the
// connection on stdin/stdout; it serves exactly one request line, "start-slot N CPU_MILLI", sets
// that CPUQuota on (and reads it back), then starts only an idle
// firecracker-vm@N.service. The authorization is the socket's owner/group/mode in fc-vm-helper.socket; no sudo/polkit rule is needed.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
)

const (
	poolFile  = "/etc/firecracker-vms/pool.conf" // root-owned, shared with fc-vm-network and vmapi
	systemctl = "/usr/bin/systemctl"
)

// handlerForPool derives privileged behavior only from root-owned pool.conf.
func handlerForPool(pool vmhelper.Pool) vmhelper.SystemdHandler {
	return vmhelper.SystemdHandler{Systemctl: systemctl, PublicEnabled: pool.PublicIPv4 != ""}
}

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "vm-helper takes no arguments")
		os.Exit(2)
	}
	pool, err := vmhelper.ReadPool(poolFile)
	if err != nil {
		fmt.Fprintf(os.Stdout, "error: pool configuration: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := vmhelper.Serve(ctx, os.Stdin, os.Stdout, handlerForPool(pool), vmhelper.Limits{Slots: pool.MaxSlots, MaxCPUMilli: pool.MaxSlotCPUMilli}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
