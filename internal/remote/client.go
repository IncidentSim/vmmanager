package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type Client struct {
	Endpoint string
	HTTP     *http.Client
}

func (c *Client) request(ctx context.Context, method, path string) (*http.Response, error) {
	r, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Endpoint, "/")+path, strings.NewReader("{}"))
	if e != nil {
		return nil, e
	}
	r.Header.Set("Content-Type", "application/json")
	resp, e := c.HTTP.Do(r)
	if e != nil {
		return nil, e
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	return resp, nil
}
func (c *Client) job(ctx context.Context, method, path string) (Job, error) {
	r, e := c.request(ctx, method, path)
	if e != nil {
		return Job{}, e
	}
	defer r.Body.Close()
	var j Job
	e = json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(&j)
	return j, e
}
func (c *Client) Start(ctx context.Context) (Job, error) { return c.job(ctx, "POST", "/v1/builds") }
func (c *Client) Get(ctx context.Context, id string) (Job, error) {
	if !validID.MatchString(id) {
		return Job{}, fmt.Errorf("invalid job ID")
	}
	return c.job(ctx, "GET", "/v1/builds/"+id)
}
func (c *Client) Cancel(ctx context.Context, id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid job ID")
	}
	_, e := c.job(ctx, "DELETE", "/v1/builds/"+id)
	return e
}

// Download streams through a fixed-size buffer and publishes only verified bytes.
// A same-directory hard link provides portable atomic no-replace semantics.
func (c *Client) Download(ctx context.Context, j Job, output string) error {
	if !validID.MatchString(j.ID) || j.Size < 0 || len(j.SHA256) != 64 {
		return fmt.Errorf("invalid artifact metadata")
	}
	if _, e := hex.DecodeString(j.SHA256); e != nil {
		return e
	}
	if _, e := os.Lstat(output); e == nil {
		return fmt.Errorf("output already exists: %s", output)
	} else if !os.IsNotExist(e) {
		return e
	}
	r, e := c.request(ctx, "GET", "/v1/builds/"+j.ID+"/image")
	if e != nil {
		return e
	}
	defer r.Body.Close()
	f, e := os.CreateTemp(filepath.Dir(output), ".download-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(r.Body, j.Size+1))
	if e != nil {
		return e
	}
	if n != j.Size || hex.EncodeToString(h.Sum(nil)) != j.SHA256 {
		return fmt.Errorf("artifact size/SHA256 mismatch")
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Link(f.Name(), output)
}
