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

	"github.com/petruekhin/vibe-firecracker/internal/identity"
	"github.com/petruekhin/vibe-firecracker/internal/jwtauth"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
)

type Config struct {
	ListenAddr       string     `json:"listen_addr"`
	TLSCertFile      string     `json:"tls_cert_file,omitempty"`
	TLSKeyFile       string     `json:"tls_key_file,omitempty"`
	StateDir         string     `json:"state_dir"`
	AllowedUserIDs   []int64    `json:"allowed_user_ids"`
	AllowedUsernames []string   `json:"allowed_usernames"`
	JWT              *JWTConfig `json:"jwt,omitempty"`
	// ExposeAPIOnTerminal serves the authenticated HTTP API and the public
	// Swagger UI through the existing HTTPS terminal gateway (same origin).
	ExposeAPIOnTerminal bool         `json:"expose_api_on_terminal,omitempty"`
	LegacyOwnerUserID   int64        `json:"legacy_owner_user_id,omitempty"`
	SharedImageIDs      []string     `json:"shared_image_ids"`
	Credentials         []Credential `json:"credentials"`
	MaxVMs              int          `json:"max_vms"`
	MaxVMsPerUser       int          `json:"max_vms_per_user"`
	DefaultProfile      string       `json:"default_profile"`
	Facade              struct {
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
		// ExposeAPI is an accepted alias of the top-level expose_api_on_terminal.
		ExposeAPI bool `json:"expose_api_on_terminal,omitempty"`
	} `json:"terminal"`
}

// JWTConfig configures verification only: the API never gets a private key.
type JWTConfig struct {
	PublicKeyFile string `json:"public_key_file"`
	Issuer        string `json:"issuer"`
	Audience      string `json:"audience"`
}

// ExposesAPI reports whether the API and Swagger UI share the terminal gateway.
func (c Config) ExposesAPI() bool { return c.ExposeAPIOnTerminal || c.Terminal.ExposeAPI }

// CanonicalUsernames returns the validated, case-folded username allowlist.
func (c Config) CanonicalUsernames() ([]string, error) {
	return identity.CanonicalList(c.AllowedUsernames)
}

// Verifier loads the JWT public key (nil when JWT is not configured).
func (c Config) Verifier() (*jwtauth.Verifier, error) {
	if c.JWT == nil {
		return nil, nil
	}
	return jwtauth.LoadVerifier(c.JWT.PublicKeyFile, c.JWT.Issuer, c.JWT.Audience)
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
	if !filepath.IsAbs(c.StateDir) || c.LegacyOwnerUserID < 0 || c.MaxVMs <= 0 || c.MaxVMs > vmbot.MaxVMsLimit || c.MaxVMsPerUser <= 0 {
		return errors.New("invalid API policy")
	}
	if len(c.AllowedUserIDs) == 0 && len(c.AllowedUsernames) == 0 {
		return errors.New("at least one allowed user ID or username is required")
	}
	if _, e := c.CanonicalUsernames(); e != nil {
		return errors.New("invalid API username allowlist")
	}
	if c.JWT == nil && len(c.Credentials) == 0 {
		return errors.New("credentials or jwt verification required")
	}
	if len(c.AllowedUsernames) > 0 && c.JWT == nil {
		return errors.New("allowed_usernames require jwt verification")
	}
	if j := c.JWT; j != nil && (!filepath.IsAbs(j.PublicKeyFile) || j.Issuer == "" || j.Audience == "") {
		return errors.New("invalid jwt configuration")
	}
	if c.ExposesAPI() && c.Terminal.PublicOrigin == "" {
		return errors.New("expose_api_on_terminal requires the terminal gateway")
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
