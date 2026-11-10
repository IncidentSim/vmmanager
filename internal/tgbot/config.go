package tgbot

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/petruekhin/vibe-firecracker/controlclient"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Version           int                      `json:"version"`
	TelegramTokenFile string                   `json:"telegram_token_file"`
	APIURL            string                   `json:"api_url"`
	APITokenFile      string                   `json:"api_token_file"`
	AllowedUserIDs    []int64                  `json:"allowed_user_ids"`
	DeliveryDir       string                   `json:"delivery_dir"`
	ImageID           string                   `json:"image_id"`
	Resources         *controlclient.Resources `json:"resources,omitempty"`
}

// uniqueObject rejects duplicate top-level keys before decoding a security
// sensitive config/offset. A duplicate offset must never silently reset the
// high-water mark via encoding/json's last-value-wins behavior.
func uniqueObject(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return false
		}
	}
	last, err := d.Token()
	if err != nil || last != json.Delim('}') {
		return false
	}
	return d.Decode(new(any)) == io.EOF
}
func strict(raw []byte, v any) error {
	if !uniqueObject(raw) {
		return errors.New("invalid or duplicate JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return errors.New("invalid JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

// privateOpen pins a regular file without following the final symlink. All
// private files must be owned by this process's uid with no group/other access.
func privateOpen(path string, flags int) (*os.File, error) {
	fd, e := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return nil, errors.New("cannot open private file")
	}
	f := os.NewFile(uintptr(fd), path)
	st, e := f.Stat()
	var fs unix.Stat_t
	se := unix.Fstat(fd, &fs)
	if e != nil || se != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || fs.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, errors.New("private file must be owned regular mode 0600")
	}
	return f, nil
}
func privateRead(path string) ([]byte, error) {
	f, e := privateOpen(path, unix.O_RDONLY)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if e != nil || len(b) > 4<<20 {
		return nil, errors.New("invalid private file size")
	}
	return b, nil
}
func LoadConfig(path string) (Config, error) {
	var c Config
	b, e := privateRead(path)
	if e != nil {
		return c, e
	}
	if strict(b, &c) != nil {
		return c, errors.New("invalid frontend config; legacy config is unsupported")
	}
	if c.Version != 1 || c.DeliveryDir == "" || !filepath.IsAbs(c.DeliveryDir) || c.ImageID == "" || c.TelegramTokenFile == "" || c.APITokenFile == "" || len(c.AllowedUserIDs) == 0 {
		return c, errors.New("frontend config requires version 1, API, private files, delivery directory, image and users")
	}
	seen := map[int64]bool{}
	for _, u := range c.AllowedUserIDs {
		if u <= 0 || seen[u] {
			return c, errors.New("allowlist must contain unique positive users")
		}
		seen[u] = true
	}
	if c.Resources != nil && (c.Resources.VCPU == 0 || c.Resources.MemoryMiB == 0) {
		return c, errors.New("invalid resources")
	}
	if _, e = controlclient.New(c.APIURL, "validation", nil); e != nil {
		return c, e
	}
	return c, nil
}
func (c Config) Secrets() (string, string, error) {
	a, e := privateRead(c.TelegramTokenFile)
	if e != nil {
		return "", "", errors.New("cannot read private Telegram token")
	}
	b, e := privateRead(c.APITokenFile)
	if e != nil {
		return "", "", errors.New("cannot read private API token")
	}
	tg, api := strings.TrimSpace(string(a)), strings.TrimSpace(string(b))
	if tg == "" || api == "" || strings.ContainsAny(tg+api, "\r\n\t ") {
		return "", "", errors.New("invalid private credentials")
	}
	return tg, api, nil
}
