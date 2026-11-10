package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/firecracker-postgres-builder/internal/remote"
)

func TestClientLogCursor(t *testing.T) {
	type snapshot struct {
		ID     string `json:"id"`
		Log    string `json:"log"`
		Offset int64  `json:"log_offset"`
		State  string `json:"state"`
	}
	const marker = "\n[server log tail; earlier bytes may have been truncated]\n"
	for _, tc := range []struct {
		name string
		logs []snapshot
		want string
	}{
		{"same prefix", []snapshot{{Log: "abc"}, {Log: "abcdef"}, {Log: "abcdef"}}, "abcdef"},
		{"full bounded tail", []snapshot{{Log: strings.Repeat("a", remote.MaxLog)}, {Log: strings.Repeat("a", remote.MaxLog-2) + "bc", Offset: 2}, {Log: strings.Repeat("a", remote.MaxLog-4) + "bcde", Offset: 4}}, strings.Repeat("a", remote.MaxLog) + "bcde"},
		{"contiguous nonoverlapping tails", []snapshot{{Log: "abc"}, {Log: "def", Offset: 3}}, "abcdef"},
		{"rolling tail", []snapshot{{Log: "abcdef"}, {Log: "cdefgh", Offset: 2}, {Log: "efghij", Offset: 4}}, "abcdefghij"},
		{"identical text at different offsets", []snapshot{{Log: "aaaa"}, {Log: "aaaa", Offset: 2}, {Log: "aaaa", Offset: 4}}, "aaaaaaaa"},
		{"genuine gap", []snapshot{{Log: "abc"}, {Log: "ghi", Offset: 6}, {Log: "ghi", Offset: 6}}, "abc" + marker + "ghi"},
		{"initial gap", []snapshot{{Log: "tail", Offset: 9}, {Log: "tail", Offset: 9}}, marker + "tail"},
		{"unicode byte cursor", []snapshot{{Log: "αβγ"}, {Log: "βγδ", Offset: 2}, {Log: "γδε", Offset: 4}}, "αβγδε"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ca, cert, key := clientPKI(t)
			cfg, err := remote.ServerTLS(ca, cert, key)
			if err != nil {
				t.Fatal(err)
			}
			i := 0
			s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if i >= len(tc.logs) {
					http.Error(w, "unexpected poll", 500)
					return
				}
				job := tc.logs[i]
				job.ID = strings.Repeat("a", 32)
				i++
				job.State = "running"
				if i == len(tc.logs) {
					job.State = "succeeded"
				}
				if err := json.NewEncoder(w).Encode(job); err != nil {
					t.Error(err)
				}
			}))
			s.TLS = cfg
			s.StartTLS()
			defer s.Close()
			var out bytes.Buffer
			if err := run(context.Background(), []string{"-endpoint", s.URL, "-tls-ca", ca, "-tls-cert", cert, "-tls-key", key}, &out); err != nil {
				t.Fatal(err)
			}
			_, log, ok := strings.Cut(out.String(), "\n")
			if !ok {
				t.Fatal("missing start message")
			}
			log, _, ok = strings.Cut(log, "\nJob ")
			if !ok {
				t.Fatal("missing success message")
			}
			if log != tc.want {
				t.Fatalf("log=%q want=%q", log, tc.want)
			}
		})
	}
}
