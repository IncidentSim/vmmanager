package controlapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
)

type Config struct {
	ListenAddr        string       `json:"listen_addr"`
	TLSCertFile       string       `json:"tls_cert_file,omitempty"`
	TLSKeyFile        string       `json:"tls_key_file,omitempty"`
	StateDir          string       `json:"state_dir"`
	AllowedUserIDs    []int64      `json:"allowed_user_ids"`
	LegacyOwnerUserID int64        `json:"legacy_owner_user_id,omitempty"`
	SharedImageIDs    []string     `json:"shared_image_ids"`
	Credentials       []Credential `json:"credentials"`
	MaxVMs            int          `json:"max_vms"`
	MaxVMsPerUser     int          `json:"max_vms_per_user"`
	DefaultProfile    string       `json:"default_profile"`
	Facade            struct {
		Addr       string `json:"addr"`
		CAFile     string `json:"ca_file"`
		CertFile   string `json:"cert_file"`
		KeyFile    string `json:"key_file"`
		ServerName string `json:"server_name"`
	} `json:"facade"`
	Terminal struct {
		ListenAddr    string `json:"listen_addr"`
		PublicOrigin  string `json:"public_origin"`
		TLSCertFile   string `json:"tls_cert_file"`
		TLSKeyFile    string `json:"tls_key_file"`
		TTYDPath      string `json:"ttyd_path"`
		SSHPath       string `json:"ssh_path"`
		SSHKeyFile    string `json:"ssh_key_file"`
		MaxClients    int    `json:"max_clients"`
		SSHUseBastion bool   `json:"ssh_use_bastion"`
	} `json:"terminal"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, e := os.Open(path)
	if e != nil {
		return c, errors.New("cannot read API configuration")
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 128*1024))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, errors.New("invalid API configuration")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("trailing configuration data")
	}
	if c.ListenAddr == "" {
		c.ListenAddr = "127.0.0.1:8090"
	}
	if c.MaxVMs == 0 {
		c.MaxVMs = 2
	}
	if c.MaxVMsPerUser == 0 {
		c.MaxVMsPerUser = 1
	}
	if c.DefaultProfile == "" {
		c.DefaultProfile = "small"
	}
	if c.Terminal.MaxClients == 0 {
		c.Terminal.MaxClients = 4
	}
	for n := range c.Credentials {
		if e = c.Credentials[n].Resolve(); e != nil {
			return c, e
		}
	}
	return c, c.Validate()
}
func (c Config) Policy() vmbot.ControlPolicy {
	return vmbot.ControlPolicy{AllowedUsers: c.AllowedUserIDs, SharedImages: c.SharedImageIDs, MaxVMs: c.MaxVMs, MaxVMsPerUser: c.MaxVMsPerUser}
}
func (c Config) Validate() error {
	if !filepath.IsAbs(c.StateDir) || c.LegacyOwnerUserID < 0 || c.MaxVMs <= 0 || c.MaxVMs > vmbot.MaxVMsLimit || c.MaxVMsPerUser <= 0 || len(c.AllowedUserIDs) == 0 || len(c.Credentials) == 0 {
		return errors.New("invalid API policy")
	}
	seen := map[int64]bool{}
	for _, u := range c.AllowedUserIDs {
		if u <= 0 || seen[u] {
			return errors.New("invalid API allowlist")
		}
		seen[u] = true
	}
	host, _, e := net.SplitHostPort(c.ListenAddr)
	if e != nil {
		return errors.New("invalid API listen address")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("both TLS files required")
	}
	if c.TLSCertFile == "" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("plaintext API must bind loopback")
		}
	}
	if c.Terminal.PublicOrigin != "" {
		host, _, e := net.SplitHostPort(c.Terminal.ListenAddr)
		if e != nil {
			return errors.New("invalid terminal listen address")
		}
		if (c.Terminal.TLSCertFile == "") != (c.Terminal.TLSKeyFile == "") {
			return errors.New("both terminal TLS files required")
		}
		if c.Terminal.TLSCertFile == "" {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return errors.New("plaintext terminal must bind loopback")
			}
		}
		if c.Terminal.MaxClients <= 0 || c.Terminal.MaxClients*c.MaxVMs > 64 {
			return errors.New("invalid terminal connection bound")
		}
	}
	return nil
}
func (c *Credential) Resolve() error {
	if c.TokenFile != "" {
		if c.SHA256 != "" {
			return errors.New("choose token file or digest")
		}
		f, e := os.Open(c.TokenFile)
		if e != nil {
			return errors.New("cannot read private credential")
		}
		defer f.Close()
		fi, e := f.Stat()
		if e != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0077 != 0 || fi.Size() > 4096 {
			return errors.New("credential must be a private regular file")
		}
		b, e := io.ReadAll(io.LimitReader(f, 4097))
		if e != nil {
			return errors.New("cannot read private credential")
		}
		token := strings.TrimSpace(string(b))
		if len(token) < 1 || strings.ContainsAny(token, "\r\n\t ") {
			return errors.New("invalid credential")
		}
		sum := sha256.Sum256([]byte(token))
		c.SHA256 = hex.EncodeToString(sum[:])
	}
	b, e := hex.DecodeString(c.SHA256)
	if e != nil || len(b) != 32 {
		return errors.New("invalid credential digest")
	}
	c.SHA256 = strings.ToLower(c.SHA256)
	return nil
}
