// Package vmlib is a public, importable library for creating Firecracker-style
// VMs from filesystem images, obtaining SSH connection metadata, stopping them
// gracefully and committing their disks as NEW offline images.
//
// It contains no PostgreSQL, Ansible or Firecracker SDK code. Orchestration lives
// in Service; host operations are behind Backend, which can be the local host
// backend (vmlib/local) or a client of a remote mTLS backend (vmlib/remote).
//
// Contexts bound a single call, never a VM's lifetime. The library installs no
// process-global signal handlers.
package vmlib

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// VMState is the facade-level lifecycle state.
type VMState string

const (
	StatePreparing VMState = "preparing"
	StateRunning   VMState = "running"
	StateStopping  VMState = "stopping"
	StateStopped   VMState = "stopped"
	StateCommitted VMState = "committed"
	StateDeleted   VMState = "deleted"
	StateFailed    VMState = "failed"
)

// Host-level states reported by a Backend.
const (
	HostPrepared = "prepared"
	HostRunning  = "running"
	HostExiting  = "exiting"
	HostExited   = "exited"
	HostRetained = "retained"
)

// SSH authentication modes.
const (
	AuthImagePreinstalled  = "image-preinstalled"
	AuthProvisionPublicKey = "provision-public-key"
	AuthClientKeyReference = "client-key-reference"
)

// Host-key policies.
const (
	HostKeyPinned         = "pinned"
	HostKeyAcceptNewPerVM = "accept-new-per-vm"
)

var (
	ErrInvalidArgument   = errors.New("vmlib: invalid argument")
	ErrNotFound          = errors.New("vmlib: not found")
	ErrAlreadyExists     = errors.New("vmlib: already exists")
	ErrResourceExhausted = errors.New("vmlib: resource exhausted")
	// ErrRetained means the working disk was deliberately kept because the VMM's
	// exit, identity or configuration could not be proven safe.
	ErrRetained = errors.New("vmlib: disk retained; VM exit not proven")
	// ErrNotStopped means an operation needs a cleanly exited VM.
	ErrNotStopped = errors.New("vmlib: VM has not cleanly stopped")
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidateID checks an opaque VM, image or profile identifier.
func ValidateID(kind, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: %s %q must match %s", ErrInvalidArgument, kind, id, idPattern)
	}
	return nil
}

var pubKeyPrefixes = []string{"ssh-ed25519 ", "ssh-rsa ", "ecdsa-sha2-nistp256 ", "ecdsa-sha2-nistp384 ", "ecdsa-sha2-nistp521 "}

// ValidatePublicKey accepts one single-line OpenSSH public key; empty is allowed.
func ValidatePublicKey(key string) error {
	if key == "" {
		return nil
	}
	if len(key) > 1024 || strings.ContainsAny(key, "\r\n\x00") {
		return fmt.Errorf("%w: public key must be one short line", ErrInvalidArgument)
	}
	for _, p := range pubKeyPrefixes {
		if strings.HasPrefix(key, p) {
			return nil
		}
	}
	return fmt.Errorf("%w: unsupported public key type", ErrInvalidArgument)
}

type Image struct {
	ID        string
	SizeBytes uint64
	SHA256    string
	Base      bool
}

type Profile struct {
	Name                  string
	VCPU, MemMiB, DiskMiB uint32
}

type Bastion struct {
	Host string
	Port int
	User string
}

// Capabilities is server policy advertised by a backend.
type Capabilities struct {
	MaxVMs      int
	Profiles    []Profile
	Bastion     *Bastion
	SSHUser     string
	SSHAuthMode string
}

// HostVM is a backend's view of one VM.
type HostVM struct {
	ID, ImageID, Profile, State string
	GuestIP                     string
	Retained                    bool
	RetainedReason              string
	HostKeyFingerprint          string
	PublicKeyProvisioned        bool
}

type ExitReceipt struct {
	VMID   string
	Clean  bool
	Detail string
}

type ReleaseResult struct {
	Released bool
	Detail   string
}

type PrepareRequest struct {
	VMID, ImageID, Profile string
	SSHPublicKey           string
}

// Backend is the narrow host-operation contract (ADR 0001 section 3). No method
// accepts a path, command, interface name, IP or socket path.
type Backend interface {
	Capabilities(ctx context.Context) (Capabilities, error)
	ListImages(ctx context.Context) ([]Image, error)
	GetImage(ctx context.Context, id string) (Image, error)
	// Prepare is idempotent by VMID and must atomically enforce admission caps.
	Prepare(ctx context.Context, req PrepareRequest) (HostVM, error)
	Start(ctx context.Context, vmID string) (HostVM, error)
	Inspect(ctx context.Context, vmID string) (HostVM, error)
	List(ctx context.Context) ([]HostVM, error)
	// RequestShutdown asks the guest to shut down gracefully; never kills.
	RequestShutdown(ctx context.Context, vmID string) (HostVM, error)
	// WaitExit returns only once the original VMM's exit receipt is valid.
	WaitExit(ctx context.Context, vmID string) (ExitReceipt, error)
	// Publish atomically creates a NEW image; it must never overwrite and must
	// refuse unless the VM's clean exit receipt is confirmed.
	Publish(ctx context.Context, vmID, newImageID string) (Image, error)
	// Release deletes the working disk and frees leases only when safe.
	Release(ctx context.Context, vmID string) (ReleaseResult, error)
}

// VM is the facade view of a VM.
type VM struct {
	ID, ImageID, Profile string
	State                VMState
	Labels               map[string]string
	Retained             bool
	RetainedReason       string
}

type CreateRequest struct {
	// ID is optional; a unique ID is generated when empty.
	ID, ImageID, Profile string
	Labels               map[string]string
	// SSHPublicKey is optionally injected into this VM's disk copy only.
	SSHPublicKey string
	// SSHKeyReference is a label the CLIENT resolves locally (never a server path).
	SSHKeyReference string
}

type SSHInfo struct {
	Host                 string
	Port                 int
	User                 string
	AuthMode             string
	KeyReference         string
	PublicKeyProvisioned bool
	HostKeyPolicy        string
	HostKeyFingerprint   string
	Bastion              *Bastion
	Ready                bool
}

type DeleteResult struct {
	Deleted  bool
	Retained bool
	Detail   string
}

// Lifecycle is the facade API implemented by Service (local or over a remote
// backend) and by the generated-gRPC facade client.
type Lifecycle interface {
	Create(ctx context.Context, req CreateRequest) (VM, error)
	Get(ctx context.Context, id string) (VM, error)
	List(ctx context.Context) ([]VM, error)
	SSHInfo(ctx context.Context, id string) (SSHInfo, error)
	Stop(ctx context.Context, id string) (VM, error)
	Commit(ctx context.Context, id, newImageID string) (Image, error)
	Delete(ctx context.Context, id string) (DeleteResult, error)
	ListImages(ctx context.Context) ([]Image, error)
}

// ProxyJump renders the value for `ssh -J`, or "" without a bastion.
func (s SSHInfo) ProxyJump() string {
	if s.Bastion == nil {
		return ""
	}
	j := s.Bastion.Host
	if s.Bastion.User != "" {
		j = s.Bastion.User + "@" + j
	}
	if s.Bastion.Port != 0 && s.Bastion.Port != 22 {
		j = fmt.Sprintf("%s:%d", j, s.Bastion.Port)
	}
	return j
}

// SSHArgs returns ssh arguments (without the "ssh" program name) for one VM.
// knownHostsFile is a caller-chosen per-VM file; keyFile is a client-local path
// the caller resolved from KeyReference ("" to omit).
func (s SSHInfo) SSHArgs(knownHostsFile, keyFile string) []string {
	var a []string
	if j := s.ProxyJump(); j != "" {
		a = append(a, "-J", j)
	}
	if keyFile != "" {
		a = append(a, "-i", keyFile)
	}
	a = append(a, "-p", fmt.Sprint(s.Port), "-o", "StrictHostKeyChecking=accept-new")
	if knownHostsFile != "" {
		a = append(a, "-o", "UserKnownHostsFile="+knownHostsFile)
	}
	return append(a, s.User+"@"+s.Host)
}

// AnsibleVars returns inventory host variables for this VM (ssh_common_args
// carries the ProxyJump). keyFile is optional and client-local.
func (s SSHInfo) AnsibleVars(keyFile string) map[string]string {
	v := map[string]string{
		"ansible_host": s.Host,
		"ansible_port": fmt.Sprint(s.Port),
		"ansible_user": s.User,
	}
	if j := s.ProxyJump(); j != "" {
		v["ansible_ssh_common_args"] = "-o ProxyJump=" + j
	}
	if keyFile != "" {
		v["ansible_ssh_private_key_file"] = keyFile
	}
	return v
}
