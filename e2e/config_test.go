package e2e

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Environment variable names (documented in docs/e2e-live.md).
const (
	envGate     = "VMAPI_E2E"
	envBaseURL  = "VMAPI_E2E_BASE_URL"
	envUserID   = "VMAPI_E2E_USER_ID"
	envTokenF   = "VMAPI_E2E_TOKEN_FILE"
	envImage    = "VMAPI_E2E_BASE_IMAGE_ID"
	envSSHKeyF  = "VMAPI_E2E_SSH_KEY_FILE"
	envDirect   = "VMAPI_E2E_SSH_DIRECT"
	envRestartU = "VMAPI_E2E_RESTART_USER_UNIT"
	envKnownF   = "VMAPI_E2E_SSH_KNOWN_HOSTS_FILE"
)

// Config never prints its Token: it has no String method and errors never
// include it.
type Config struct {
	BaseURL     string
	Token       string
	UserID      int64
	BaseImageID string
	SSHKeyFile  string
	RestartUnit string // empty: restart test skipped
	// KnownHostsFile is an optional operator-trusted known_hosts file. When
	// set it is the sole host-key trust source; its contents are never read
	// into logs.
	KnownHostsFile string
}

var (
	imageIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	unitPattern    = regexp.MustCompile(`^[A-Za-z0-9:_.@-]{1,100}\.service$`)
)

// loadConfig validates every setting before any request is made. Errors name
// variables only, never values.
func loadConfig(getenv func(string) string) (Config, error) {
	var missing []string
	need := func(k string) string {
		v := strings.TrimSpace(getenv(k))
		if v == "" {
			missing = append(missing, k)
		}
		return v
	}
	base, uid, tokf, img, keyf := need(envBaseURL), need(envUserID), need(envTokenF), need(envImage), need(envSSHKeyF)
	direct := getenv(envDirect)
	if direct != "1" {
		missing = append(missing, envDirect+"=1")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment: %s", strings.Join(missing, ", "))
	}
	c := Config{BaseURL: strings.TrimRight(base, "/"), BaseImageID: img, SSHKeyFile: keyf, RestartUnit: strings.TrimSpace(getenv(envRestartU))}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, fmt.Errorf("%s must be a plain https://host[:port] URL", envBaseURL)
	}
	if c.UserID, err = strconv.ParseInt(uid, 10, 64); err != nil || c.UserID <= 0 {
		return Config{}, fmt.Errorf("%s must be a positive integer", envUserID)
	}
	if !imageIDPattern.MatchString(img) {
		return Config{}, fmt.Errorf("%s is not a valid image ID", envImage)
	}
	if c.RestartUnit != "" && !unitPattern.MatchString(c.RestartUnit) {
		return Config{}, fmt.Errorf("%s must be a plain NAME.service unit", envRestartU)
	}
	if kh := strings.TrimSpace(getenv(envKnownF)); kh != "" {
		if err := checkReadableRegular(kh); err != nil {
			return Config{}, fmt.Errorf("%s must be a readable regular file", envKnownF)
		}
		c.KnownHostsFile = kh
	}
	b, err := os.ReadFile(tokf)
	if err != nil {
		return Config{}, fmt.Errorf("%s: cannot read file", envTokenF)
	}
	c.Token = strings.TrimSpace(string(b))
	if c.Token == "" || strings.ContainsAny(c.Token, " \t\r\n") {
		return Config{}, errors.New(envTokenF + ": file must hold exactly one token")
	}
	st, err := os.Stat(keyf)
	if err != nil || !st.Mode().IsRegular() {
		return Config{}, fmt.Errorf("%s must be a regular file", envSSHKeyF)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return Config{}, fmt.Errorf("%s must not be group/world accessible (chmod 600)", envSSHKeyF)
	}
	return c, nil
}

// checkReadableRegular verifies path is a regular file (symlinks resolved)
// that can be opened for reading. It never reads or returns contents.
func checkReadableRegular(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}
