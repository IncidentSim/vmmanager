// vm-helper-client is the unprivileged side: the program vmapi's backend `helper` setting
// points at. It speaks the one-line protocol over the fixed helper socket. It has
// no sudo, no flags and no way to name a unit, path or command.
//
//	vm-helper-client start-slot N
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
)

func run(args []string, dial func() (io.ReadWriteCloser, error)) error {
	if len(args) != 2 || args[0] != "start-slot" {
		return errors.New("usage: vm-helper-client start-slot N")
	}
	slot, err := strconv.Atoi(args[1])
	if err != nil || strconv.Itoa(slot) != args[1] {
		return fmt.Errorf("invalid slot %q", args[1])
	}
	if slot < 0 || slot >= vmhelper.MaxSlots {
		return fmt.Errorf("slot %d out of range", slot)
	}
	conn, err := dial()
	if err != nil {
		return fmt.Errorf("connect to helper: %w", err)
	}
	defer conn.Close()
	return vmhelper.Request(conn, slot)
}

func main() {
	err := run(os.Args[1:], func() (io.ReadWriteCloser, error) {
		return net.DialTimeout("unix", vmhelper.SocketPath, 5*time.Second)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}
