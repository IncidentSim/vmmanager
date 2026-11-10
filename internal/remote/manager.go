// Package remote implements a single trusted-user build manager, not a VMM proxy.
package remote

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const MaxLog = 65536
const MaxJobDuration = 30 * time.Minute

var validID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Job struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Log   string `json:"log"`
	// LogOffset is the absolute byte offset of Log in the normalized UTF-8 stream.
	LogOffset int64  `json:"log_offset"`
	Error     string `json:"error,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Size      int64  `json:"size,omitempty"`
}
type Runner func(context.Context, string, io.Writer) error
type entry struct {
	Job
	path   string
	cancel context.CancelFunc
	done   chan struct{}
}
type Manager struct {
	mu         sync.Mutex
	dir        string
	run        Runner
	jobs       map[string]*entry
	active     *entry
	closed     bool
	jobTimeout time.Duration
}

func NewManager(dir string, run Runner) (*Manager, error) {
	if run == nil {
		return nil, fmt.Errorf("missing runner")
	}
	abs, e := filepath.Abs(dir)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(abs, 0700); e != nil {
		return nil, e
	}
	return &Manager{dir: abs, run: run, jobs: make(map[string]*entry), jobTimeout: MaxJobDuration}, nil
}

// Close waits without forced kills: the child must finish its receipt-based cleanup.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	j := m.active
	if j != nil {
		j.cancel()
	}
	m.mu.Unlock()
	if j != nil {
		<-j.done
	}
}

type logWriter struct {
	m *Manager
	j *entry
}

func (w logWriter) Write(p []byte) (int, error) {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	// Normalize each write before counting bytes so JSON encoding cannot change
	// the cursor. Invalid sequences (including runes split across writes) are
	// replaced here, rather than being repaired later by encoding/json.
	s := w.j.Log + strings.ToValidUTF8(string(p), "\ufffd")
	if len(s) > MaxLog {
		start := len(s) - MaxLog
		for start < len(s) && !utf8.RuneStart(s[start]) {
			start++
		}
		w.j.LogOffset += int64(start)
		s = s[start:]
	}
	w.j.Log = s
	return len(p), nil
}
func (m *Manager) execute(ctx context.Context, j *entry) {
	err := m.run(ctx, j.path, logWriter{m, j})
	var hash string
	var size int64
	if err == nil && ctx.Err() == nil {
		f, e := os.Open(j.path)
		if e == nil {
			h := sha256.New()
			size, e = io.Copy(h, f)
			ce := f.Close()
			if e == nil {
				e = ce
			}
			hash = hex.EncodeToString(h.Sum(nil))
		}
		err = e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	defer close(j.done)
	defer j.cancel()
	if ctx.Err() != nil {
		j.State = "cancelled"
		j.Error = ctx.Err().Error()
		if err != nil {
			j.Error = err.Error()
		}
	} else if err != nil {
		j.State = "failed"
		j.Error = err.Error()
	} else {
		j.State = "succeeded"
		j.SHA256 = hash
		j.Size = size
	}
	m.active = nil
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Do not use ServeMux path cleaning: reject traversal rather than redirect it.
	if r.URL.RawQuery != "" {
		http.Error(w, "query not supported", 400)
		return
	}
	if r.URL.Path == "/v1/builds" && r.Method == http.MethodPost {
		body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		if e != nil || strings.TrimSpace(string(body)) != "{}" {
			http.Error(w, "expected empty JSON object {}", 400)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closed || m.active != nil {
			http.Error(w, "builder busy or shutting down", 409)
			return
		}
		b := make([]byte, 16)
		if _, e = rand.Read(b); e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		id := hex.EncodeToString(b)
		dir, e := os.MkdirTemp(m.dir, id+"-")
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), m.jobTimeout)
		j := &entry{Job: Job{ID: id, State: "running"}, path: filepath.Join(dir, "image.ext4"), cancel: cancel, done: make(chan struct{})}
		m.jobs[id] = j
		m.active = j
		reply(w, 202, j.Job)
		go m.execute(ctx, j)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/builds/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/v1/builds/") || len(parts) > 2 || !validID.MatchString(parts[0]) {
		http.Error(w, "invalid route or job ID", 400)
		return
	}
	m.mu.Lock()
	j := m.jobs[parts[0]]
	if j == nil {
		m.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 && parts[1] == "image" && r.Method == http.MethodGet {
		if j.State != "succeeded" {
			m.mu.Unlock()
			http.Error(w, "image unavailable", 409)
			return
		}
		path := j.path
		hash := j.SHA256
		m.mu.Unlock()
		f, e := os.Open(path)
		if e != nil {
			http.Error(w, "image unavailable", 500)
			return
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			http.Error(w, "image unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-SHA256", hash)
		http.ServeContent(w, r, "image.ext4", st.ModTime(), f)
		return
	}
	defer m.mu.Unlock()
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		reply(w, 200, j.Job)
	case http.MethodDelete:
		if j.State == "running" {
			j.State = "cancelling"
			j.cancel()
		}
		reply(w, 202, j.Job)
	default:
		http.Error(w, "method not allowed", 405)
	}
}
