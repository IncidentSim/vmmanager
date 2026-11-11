package vmnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
)

// ExecRunner runs real commands with a fixed trusted PATH and C locale.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, stdin, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return Result{out.String(), errb.String(), ee.ExitCode()}, nil
	}
	if err != nil {
		return Result{}, err
	}
	return Result{Stdout: out.String(), Stderr: errb.String()}, nil
}

// Usage is the command synopsis.
const Usage = "usage: vm-network check|up|refresh|down|flush-conntrack N"

// Main implements the CLI: args are the action and the slot number. It reads
// only poolConf (never evaluating it) and, except for check, requires root and
// serializes on StateDir/.lock.
func Main(ctx context.Context, args []string, poolConf, stateDir string, run Runner, euid int) error {
	pool, err := readPool(poolConf)
	if err != nil {
		return err
	}
	if len(args) != 2 {
		return errors.New(Usage)
	}
	action := args[0]
	if !slotPattern.MatchString(args[1]) {
		return errors.New(Usage)
	}
	n, _ := strconv.Atoi(args[1])
	slot, err := New(pool, n, run, stateDir)
	if err != nil {
		return errors.New(Usage)
	}
	switch action {
	case "check":
		return slot.CheckCollisions(ctx)
	case "up", "refresh", "down", "flush-conntrack":
	default:
		return errors.New(Usage)
	}
	if euid != 0 {
		return errors.New("Requires root")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(stateDir+"/.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	switch action {
	case "up":
		return slot.Up(ctx)
	case "refresh":
		return slot.Refresh(ctx)
	case "down":
		return slot.Down(ctx)
	default:
		return slot.FlushEndpoint(ctx)
	}
}

var slotPattern = regexp.MustCompile(`^(0|[1-9][0-9]?)$`)

// readPool refuses unreadable files and NUL bytes (the Go parser rejects them).
func readPool(path string) (vmhelper.Pool, error) {
	p, err := vmhelper.ReadPool(path)
	if err != nil {
		return vmhelper.Pool{}, fmt.Errorf("pool.conf: %w", err)
	}
	return p, nil
}
