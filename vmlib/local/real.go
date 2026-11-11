//go:build linux

package local

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/petruekhin/vibe-firecracker/vmlib"

	"github.com/firecracker-microvm/firecracker-go-sdk/client"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	"github.com/sirupsen/logrus"
)

// HelperLauncher invokes an operator-installed, root-owned helper by absolute
// path with exactly three arguments: the fixed verb "start-slot", a bounded
// slot integer and the VMM CPU quota in millicores (the helper turns it into the
// unit's systemd CPUQuota=). It adds no shell, no sudo and no caller-supplied text. How the
// helper obtains privilege (and which units it can touch) is an operator
// decision documented in deployment/multi-vm; it is not performed here.
type HelperLauncher struct{ path string }

func NewHelperLauncher(path string) (*HelperLauncher, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("helper path must be absolute")
	}
	return &HelperLauncher{path: path}, nil
}

func (h *HelperLauncher) StartSlot(ctx context.Context, slot int, cpuMilli uint32) error {
	if slot < 0 || slot >= maxSlots {
		return fmt.Errorf("slot %d out of range", slot)
	}
	if err := vmlib.ValidateCPULimitMilli(cpuMilli); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, h.path, "start-slot", strconv.Itoa(slot), strconv.FormatUint(uint64(cpuMilli), 10)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("helper start-slot %d: %w: %s", slot, err, out)
	}
	return nil
}

// SocketPath is the slot's Firecracker API socket, derived from the slot only.
func SocketPath(runDir string, slot int) string {
	return filepath.Join(runDir, fmt.Sprintf("slot-%d", slot), "api.sock")
}

// SDKVMM controls per-slot VMMs through firecracker-go-sdk's HTTP client.
// It starts no process and never signals one.
type SDKVMM struct{ RunDir string }

var _ VMM = SDKVMM{}

func (v SDKVMM) client(slot int) *firecracker.Client {
	logger := logrus.NewEntry(logrus.New())
	socket := SocketPath(v.RunDir, slot)
	// These SDK clients are short-lived. The SDK's default transport retains
	// idle sockets indefinitely, eventually exhausting Firecracker's connection
	// limit. Own the transport and close each connection after its response,
	// including decoded API errors; no client cache or cleanup owner is needed.
	transport := httptransport.New(client.DefaultHost, client.DefaultBasePath, client.DefaultSchemes)
	transport.Transport = &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	transport.SetLogger(logger)
	// Keep the SDK's typed methods and timeout/context handling, replacing only
	// their generated operations client's transport. Do not add a detached
	// context or an http.Client.Timeout that overrides caller/SDK deadlines.
	return firecracker.NewClient(socket, logger, false,
		firecracker.WithOpsClient(operations.New(transport, strfmt.Default)))
}

func (v SDKVMM) Info(ctx context.Context, slot int) (VMMInfo, error) {
	r, err := v.client(slot).GetInstanceInfo(ctx)
	if err != nil {
		return VMMInfo{}, err
	}
	if r.Payload == nil || r.Payload.State == nil || r.Payload.ID == nil {
		return VMMInfo{}, errors.New("incomplete VMM instance info")
	}
	return VMMInfo{ID: *r.Payload.ID, State: *r.Payload.State}, nil
}

func (v SDKVMM) Configure(ctx context.Context, slot int, s MachineSpec) error {
	c := v.client(slot)
	if _, err := c.PutMachineConfiguration(ctx, &models.MachineConfiguration{
		VcpuCount: firecracker.Int64(int64(s.VCPU)), MemSizeMib: firecracker.Int64(int64(s.MemMiB)), Smt: firecracker.Bool(false)}); err != nil {
		return err
	}
	if _, err := c.PutGuestBootSource(ctx, &models.BootSource{KernelImagePath: firecracker.String(s.Kernel), BootArgs: s.BootArgs}); err != nil {
		return err
	}
	if _, err := c.PutGuestDriveByID(ctx, "rootfs", &models.Drive{DriveID: firecracker.String("rootfs"),
		PathOnHost: firecracker.String(s.DiskJailPath), IsRootDevice: firecracker.Bool(true), IsReadOnly: firecracker.Bool(false)}); err != nil {
		return err
	}
	_, err := c.PutGuestNetworkInterfaceByID(ctx, "eth0", &models.NetworkInterface{IfaceID: firecracker.String("eth0"),
		HostDevName: firecracker.String(s.Tap), GuestMac: s.MAC})
	return err
}

func (v SDKVMM) InstanceStart(ctx context.Context, slot int) error {
	_, err := v.client(slot).CreateSyncAction(ctx, &models.InstanceActionInfo{ActionType: firecracker.String("InstanceStart")})
	return err
}

func (v SDKVMM) CtrlAltDel(ctx context.Context, slot int) error {
	_, err := v.client(slot).CreateSyncAction(ctx, &models.InstanceActionInfo{ActionType: firecracker.String("SendCtrlAltDel")})
	return err
}
