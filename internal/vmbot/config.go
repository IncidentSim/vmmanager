package vmbot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// Config is the non-secret JSON configuration. The Telegram token lives in a
// separate 0600 file named by TelegramTokenFile.
type Config struct {
	TelegramTokenFile string         `json:"telegram_token_file"`
	AllowedUserID     int64          `json:"allowed_user_id"`
	Facade            FacadeConfig   `json:"facade"`
	VM                VMConfig       `json:"vm"`
	MaxVMs            int            `json:"max_vms"` // application ceiling, default 2; backend policy stays authoritative
	StateDir          string         `json:"state_dir"`
	Terminal          TerminalConfig `json:"terminal"`
}

type FacadeConfig struct {
	Addr       string `json:"addr"`
	ServerName string `json:"server_name"`
	CAFile     string `json:"ca_file"`
	CertFile   string `json:"cert_file"`
	KeyFile    string `json:"key_file"`
}

type VMConfig struct {
	ImageID string `json:"image_id"`
	Profile string `json:"profile"`
}

type TerminalConfig struct {
	ListenAddr   string `json:"listen_addr"`
	PublicOrigin string `json:"public_origin"` // scheme://host[:port], exact match for Host/Origin
	TLSCertFile  string `json:"tls_cert_file"`
	TLSKeyFile   string `json:"tls_key_file"`
	TTYDPath     string `json:"ttyd_path"`
	SSHPath      string `json:"ssh_path"`
	SSHKeyFile   string `json:"ssh_key_file"`
	// SSHUseBastion honours the bastion advertised by the daemon; default is a
	// direct connection to the guest address.
	SSHUseBastion          bool `json:"ssh_use_bastion"`
	MaxClients             int  `json:"max_clients"`
	InsecureHTTPForTesting bool `json:"insecure_http_for_testing"`
}

// LoadConfig reads and validates the configuration (strict: unknown fields fail).
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if c.Terminal.TTYDPath == "" {
		c.Terminal.TTYDPath = "ttyd"
	}
	if c.Terminal.SSHPath == "" {
		c.Terminal.SSHPath = "ssh"
	}
	if c.Terminal.MaxClients == 0 {
		c.Terminal.MaxClients = 2
	}
	if c.MaxVMs == 0 {
		c.MaxVMs = DefaultMaxVMs
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c Config) validate() error {
	for name, p := range map[string]string{"telegram_token_file": c.TelegramTokenFile, "state_dir": c.StateDir,
		"facade.ca_file": c.Facade.CAFile, "facade.cert_file": c.Facade.CertFile, "facade.key_file": c.Facade.KeyFile,
		"terminal.ssh_key_file": c.Terminal.SSHKeyFile} {
		if p == "" || !filepath.IsAbs(p) {
			return fmt.Errorf("%s must be an absolute path", name)
		}
	}
	if c.AllowedUserID <= 0 {
		return errors.New("allowed_user_id must be a positive Telegram user ID")
	}
	if c.Facade.Addr == "" || c.Facade.ServerName == "" {
		return errors.New("facade.addr and facade.server_name are required")
	}
	if err := vmlib.ValidateID("image", c.VM.ImageID); err != nil {
		return err
	}
	if err := vmlib.ValidateID("profile", c.VM.Profile); err != nil {
		return err
	}
	t := c.Terminal
	if t.MaxClients < 1 || t.MaxClients > 8 {
		return errors.New("terminal.max_clients must be 1..8")
	}
	if c.MaxVMs < 1 || c.MaxVMs > MaxVMsLimit {
		return fmt.Errorf("max_vms must be 1..%d", MaxVMsLimit)
	}
	if c.MaxVMs*t.MaxClients > maxGatewayConns {
		return fmt.Errorf("max_vms*terminal.max_clients must not exceed %d (gateway connection bound)", maxGatewayConns)
	}
	if _, _, err := net.SplitHostPort(t.ListenAddr); err != nil {
		return fmt.Errorf("terminal.listen_addr: %w", err)
	}
	u, err := url.Parse(t.PublicOrigin)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("terminal.public_origin must be scheme://host[:port] only")
	}
	if (t.TLSCertFile == "") != (t.TLSKeyFile == "") {
		return errors.New("terminal.tls_cert_file and tls_key_file go together")
	}
	switch u.Scheme {
	case "https":
		if t.TLSCertFile == "" && !isLoopbackListen(t.ListenAddr) {
			return errors.New("an https public_origin without tls_cert_file/tls_key_file requires a loopback listen_addr behind an HTTPS proxy")
		}
	case "http":
		if !t.InsecureHTTPForTesting {
			return errors.New("public_origin must be https (set insecure_http_for_testing only for local tests)")
		}
		if !isLoopbackListen(t.ListenAddr) || t.TLSCertFile != "" {
			return errors.New("insecure http is only allowed on a loopback listener without TLS files")
		}
	default:
		return errors.New("terminal.public_origin scheme must be https")
	}
	for _, p := range []string{t.TLSCertFile, t.TLSKeyFile} {
		if p != "" && !filepath.IsAbs(p) {
			return errors.New("terminal TLS files must be absolute paths")
		}
	}
	return nil
}

// ReadToken reads the bot token, refusing files other users can read.
func (c Config) ReadToken() (string, error) {
	fi, err := os.Stat(c.TelegramTokenFile)
	if err != nil {
		return "", fmt.Errorf("telegram token file: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("telegram token file must be mode 0600 (is %o)", fi.Mode().Perm())
	}
	b, err := os.ReadFile(c.TelegramTokenFile)
	if err != nil {
		return "", fmt.Errorf("telegram token file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.ContainsAny(tok, " \t\r\n/") {
		return "", errors.New("telegram token file is empty or malformed")
	}
	return tok, nil
}
