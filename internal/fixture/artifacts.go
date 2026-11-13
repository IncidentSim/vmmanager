// Package fixture downloads the pinned Firecracker artifacts and converts the
// upstream Ubuntu CI rootfs into a base image the product's offline
// provisioners accept. It is shared by the E2E suite and the `vmbundle` example builder.
package fixture

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed artifacts.json
var pinned []byte

// Pinned returns the manifest of the artifacts shipped in this binary.
func Pinned() (Manifest, error) { return parseManifest(pinned) }

// Artifact is one pinned download.
type Artifact struct {
	Name   string `json:"name"`   // destination file name inside the output directory
	URL    string `json:"url"`    // https only
	SHA256 string `json:"sha256"` // of the downloaded file; must be pinned
	Bytes  int64  `json:"bytes"`  // hard upper bound for the download
	// Member, when set, names the single regular file extracted from a .tgz
	// archive (the archive hash is what is pinned).
	Member string `json:"member,omitempty"`
	Mode   uint32 `json:"mode,omitempty"` // extracted file mode (default 0644)
}

// Manifest lists the pinned upstream artifacts (E2E job and example bundle).
type Manifest struct {
	Artifacts []Artifact `json:"artifacts"`
}

var (
	hex64    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	baseName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// LoadManifest strictly decodes a manifest file.
func LoadManifest(p string) (Manifest, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return Manifest{}, err
	}
	return parseManifest(b)
}

func parseManifest(b []byte) (Manifest, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Manifest{}, errors.New("manifest: trailing data")
	}
	return m, m.Validate()
}

// Validate refuses unpinned or unsafe entries. An artifact without a real
// SHA-256 can never be fetched: the job must fail, not trust what it downloads.
func (m Manifest) Validate() error {
	if len(m.Artifacts) == 0 {
		return errors.New("manifest has no artifacts")
	}
	seen := map[string]bool{}
	for _, a := range m.Artifacts {
		if !baseName.MatchString(a.Name) || seen[a.Name] {
			return fmt.Errorf("artifact name %q is invalid or duplicated", a.Name)
		}
		seen[a.Name] = true
		u, err := url.Parse(a.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return fmt.Errorf("artifact %s: URL must be plain https", a.Name)
		}
		if !hex64.MatchString(a.SHA256) {
			return fmt.Errorf("artifact %s: sha256 is not pinned (64 lowercase hex digits required); pin it in the manifest after verifying the release", a.Name)
		}
		if a.Bytes <= 0 || a.Bytes > 4<<30 {
			return fmt.Errorf("artifact %s: bytes must be a positive bound up to 4 GiB", a.Name)
		}
		if a.Mode != 0 && a.Mode != 0644 && a.Mode != 0755 {
			return fmt.Errorf("artifact %s: unsafe mode", a.Name)
		}
		if a.Member != "" && (path.Clean(a.Member) != a.Member || strings.HasPrefix(a.Member, "/") || strings.HasPrefix(a.Member, "..")) {
			return fmt.Errorf("artifact %s: unsafe archive member %q", a.Name, a.Member)
		}
	}
	return nil
}

// Fetch downloads every artifact into dir (created 0700), verifying size bound
// and SHA-256 before anything becomes visible under its final name.
func Fetch(ctx context.Context, m Manifest, dir string, client *http.Client) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if client == nil {
		client = &http.Client{}
	}
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("refusing redirect to a non-https URL")
		}
		if len(via) > 5 {
			return errors.New("too many redirects")
		}
		return nil
	}
	for _, a := range m.Artifacts {
		if err := fetchOne(ctx, &c, a, dir); err != nil {
			return fmt.Errorf("%s: %w", a.Name, err)
		}
	}
	return nil
}

func fetchOne(ctx context.Context, c *http.Client, a Artifact, dir string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, a.Bytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > a.Bytes {
		return fmt.Errorf("download exceeds the pinned bound of %d bytes", a.Bytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return fmt.Errorf("sha256 mismatch: got %s, pinned %s", got, a.SHA256)
	}
	final := filepath.Join(dir, a.Name)
	if a.Member == "" {
		mode := os.FileMode(a.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := os.Chmod(tmp.Name(), mode); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), final)
	}
	return extractMember(tmp.Name(), a, final)
}

// extractMember copies exactly one regular file out of a verified .tgz.
func extractMember(archive string, a Artifact, final string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("member %q not found in archive", a.Member)
		}
		if err != nil {
			return err
		}
		if path.Clean(hdr.Name) != a.Member {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("member %q is not a regular file", a.Member)
		}
		mode := os.FileMode(a.Mode)
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.CreateTemp(filepath.Dir(final), ".extract-*")
		if err != nil {
			return err
		}
		defer os.Remove(out.Name())
		n, err := io.Copy(out, io.LimitReader(tr, 4<<30+1))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if n > 4<<30 {
			return errors.New("archive member too large")
		}
		if err := os.Chmod(out.Name(), mode); err != nil {
			return err
		}
		return os.Rename(out.Name(), final)
	}
}
