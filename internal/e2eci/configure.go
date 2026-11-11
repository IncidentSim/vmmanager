package e2eci

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
	"github.com/petruekhin/vibe-firecracker/internal/vmnet"
)

// Settings describe the disposable E2E host. Zero values get defaults.
type Settings struct {
	Dir         string // private directory owned by the runner user (0700)
	UserID      int64  // numeric E2E principal (default 424242)
	StorageRoot string // existing storage mount (default /var/lib/firecracker-builder/storage)
	Libexec     string // directory with fc-vm-helper-client (default /usr/local/libexec)
	PoolConf    string // path the root-owned pool.conf will be installed at
	RunDir      string // default /run/firecracker-vms
	Listen      string // https listen address (default 127.0.0.1:18443)
	ImageID     string // base image ID (default e2e-base)
	SSHUser     string // guest login user (default root)
	GuestDNS    []string
	MaxVMs      int // default 2
	// Prefixes are candidate GUEST_PREFIX values tried in order.
	Prefixes []string
	// Runner executes `ip` for collision checks (default real).
	Runner vmnet.Runner
}

func (s Settings) withDefaults() Settings {
	if s.UserID == 0 {
		s.UserID = 424242
	}
	if s.StorageRoot == "" {
		s.StorageRoot = "/var/lib/firecracker-builder/storage"
	}
	if s.Libexec == "" {
		s.Libexec = "/usr/local/libexec"
	}
	if s.PoolConf == "" {
		s.PoolConf = "/etc/firecracker-vms/pool.conf"
	}
	if s.RunDir == "" {
		s.RunDir = "/run/firecracker-vms"
	}
	if s.Listen == "" {
		s.Listen = "127.0.0.1:18443"
	}
	if s.ImageID == "" {
		s.ImageID = "e2e-base"
	}
	if s.SSHUser == "" {
		s.SSHUser = "root"
	}
	if len(s.GuestDNS) == 0 {
		s.GuestDNS = []string{"1.1.1.1"}
	}
	if s.MaxVMs == 0 {
		s.MaxVMs = 2
	}
	if len(s.Prefixes) == 0 {
		s.Prefixes = []string{"10.213", "10.214", "10.215", "10.216", "10.217", "10.218"}
	}
	if s.Runner == nil {
		s.Runner = vmnet.ExecRunner{}
	}
	return s
}

// Result lists what Configure produced.
type Result struct {
	PoolConf   string // file to install root-owned at Settings.PoolConf
	Prefix     string
	APIConfig  string // vmapi JSON
	EnvFile    string // KEY=VALUE lines for the e2e suite
	CAFile     string
	TokenFile  string
	ImagesDir  string
	WorkRoot   string
	ListenAddr string
}

// ChoosePrefix returns the first candidate prefix that overlaps no route or
// interface address of any slot.
func ChoosePrefix(ctx context.Context, s Settings) (string, error) {
	s = s.withDefaults()
	var last error
	for _, p := range s.Prefixes {
		pool, err := vmhelper.ParsePool([]byte(fmt.Sprintf("GUEST_PREFIX=%s\nMAX_SLOTS=%d\n", p, s.MaxVMs)))
		if err != nil {
			return "", err
		}
		ok := true
		for n := 0; n < pool.MaxSlots && ok; n++ {
			slot, err := vmnet.New(pool, n, s.Runner, "")
			if err != nil {
				return "", err
			}
			if err := slot.CheckCollisions(ctx); err != nil {
				ok, last = false, err
			}
		}
		if ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("no collision-free guest prefix among %v: %w", s.Prefixes, last)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}

// Configure writes pool.conf (to be installed root-owned), the vmapi
// configuration, a self-signed CA and server certificate for 127.0.0.1, an
// owner token and a 32-byte SSH master key, all under s.Dir. It uses fresh
// random material per run and never reads or writes anything outside s.Dir.
func Configure(ctx context.Context, s Settings) (*Result, error) {
	s = s.withDefaults()
	if !filepath.IsAbs(s.Dir) || filepath.Clean(s.Dir) != s.Dir {
		return nil, errors.New("work directory must be a clean absolute path")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return nil, err
	}
	for _, d := range []string{"state", "secrets", "ssh-store", "tls"} {
		if err := os.MkdirAll(filepath.Join(s.Dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	prefix, err := ChoosePrefix(ctx, s)
	if err != nil {
		return nil, err
	}
	r := &Result{
		Prefix:     prefix,
		PoolConf:   filepath.Join(s.Dir, "pool.conf"),
		APIConfig:  filepath.Join(s.Dir, "vmapi.json"),
		EnvFile:    filepath.Join(s.Dir, "e2e.env"),
		CAFile:     filepath.Join(s.Dir, "tls", "ca.pem"),
		TokenFile:  filepath.Join(s.Dir, "secrets", "owner-token"),
		ImagesDir:  s.StorageRoot + "/vms/images",
		WorkRoot:   s.StorageRoot + "/vms/work",
		ListenAddr: s.Listen,
	}
	if err := writeFile(r.PoolConf, []byte(fmt.Sprintf("GUEST_PREFIX=%s\nMAX_SLOTS=%d\n", prefix, s.MaxVMs)), 0o644); err != nil {
		return nil, err
	}
	token, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	if err := writeFile(r.TokenFile, []byte(token+"\n"), 0o600); err != nil {
		return nil, err
	}
	master := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, master); err != nil {
		return nil, err
	}
	masterFile := filepath.Join(s.Dir, "secrets", "ssh-master.key")
	if err := writeFile(masterFile, master, 0o600); err != nil {
		return nil, err
	}
	certFile, keyFile := filepath.Join(s.Dir, "tls", "server.pem"), filepath.Join(s.Dir, "tls", "server.key")
	host, _, err := net.SplitHostPort(s.Listen)
	if err != nil {
		return nil, err
	}
	if err := writeTLS(r.CAFile, certFile, keyFile, host); err != nil {
		return nil, err
	}
	backend := map[string]any{
		"work_root": r.WorkRoot, "images_dir": r.ImagesDir, "run_dir": s.RunDir, "storage_mount": s.StorageRoot,
		"max_vms": s.MaxVMs, "helper": s.Libexec + "/fc-vm-helper-client", "pool_conf": s.PoolConf,
		"guest_prefix": prefix, "guest_dns": s.GuestDNS, "guest_network_provisioning": "ubuntu-fcnet",
		"ssh_user": s.SSHUser, "min_free_mib": 1024,
		"max_total_mem_mib": 1024 * s.MaxVMs / 2, "max_total_vcpu": s.MaxVMs, "unit_mem_limit_mib": 768, "vmm_overhead_mib": 64,
		"profiles": []map[string]any{{"name": "small", "vcpu": 1, "mem_mib": 512, "disk_mib": 2048}},
		"personal_ssh": map[string]any{
			"enabled": true, "store_dir": filepath.Join(s.Dir, "ssh-store"), "master_key_file": masterFile,
		},
	}
	api := map[string]any{
		"listen_addr": s.Listen, "tls_cert_file": certFile, "tls_key_file": keyFile,
		"state_dir": filepath.Join(s.Dir, "state"), "allowed_user_ids": []int64{s.UserID},
		"shared_image_ids": []string{s.ImageID}, "max_vms": s.MaxVMs, "max_vms_per_user": s.MaxVMs,
		"default_profile": "small", "idle_timeout_seconds": 3600,
		"credentials": []map[string]any{{"token_file": r.TokenFile, "user_id": s.UserID}},
		"backend":     backend,
	}
	data, err := json.MarshalIndent(api, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFile(r.APIConfig, append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	env := strings.Join([]string{
		"VMAPI_E2E=1",
		"VMAPI_E2E_BASE_URL=https://" + s.Listen,
		fmt.Sprintf("VMAPI_E2E_USER_ID=%d", s.UserID),
		"VMAPI_E2E_TOKEN_FILE=" + r.TokenFile,
		"VMAPI_E2E_BASE_IMAGE_ID=" + s.ImageID,
		"VMAPI_E2E_SSH_DIRECT=1",
		"SSL_CERT_FILE=" + r.CAFile,
	}, "\n") + "\n"
	if err := writeFile(r.EnvFile, []byte(env), 0o600); err != nil {
		return nil, err
	}
	return r, nil
}

// writeTLS creates a throwaway CA and a server certificate for host (an IP or
// name) valid for one day.
func writeTLS(caFile, certFile, keyFile, host string) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "vm-e2e-ci-ca"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &srvKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		return err
	}
	if err := writeFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644); err != nil {
		return err
	}
	if err := writeFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return writeFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
}
