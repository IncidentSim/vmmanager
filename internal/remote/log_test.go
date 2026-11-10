package remote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLogTailAbsoluteUTF8Offsets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fragments []string
	}{
		{"ascii rolling and identical text", []string{strings.Repeat("a", MaxLog), "aa", "aa", strings.Repeat("b", MaxLog+17)}},
		{"unicode boundary", []string{"α" + strings.Repeat("x", MaxLog-2), "y", "界", "🙂"}},
		{"invalid and split write fragments", []string{strings.Repeat("x", MaxLog-1), "\xff\xfe", "\xe2", "\x82\xac", "世界"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := strings.Repeat("a", 32)
			j := &entry{Job: Job{ID: id, State: "running"}}
			m := &Manager{jobs: map[string]*entry{id: j}}
			writer := logWriter{m, j}
			var stream string
			for i, fragment := range tc.fragments {
				n, err := writer.Write([]byte(fragment))
				if err != nil || n != len(fragment) {
					t.Fatalf("write %d: %d, %v", i, n, err)
				}
				stream += strings.ToValidUTF8(fragment, "\ufffd")
				start := len(stream) - MaxLog
				if start < 0 {
					start = 0
				}
				for start < len(stream) && !utf8.RuneStart(stream[start]) {
					start++
				}
				response := httptest.NewRecorder()
				m.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/builds/"+id, nil))
				var wire struct {
					Log       string `json:"log"`
					LogOffset int64  `json:"log_offset"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
					t.Fatal(err)
				}
				if wire.LogOffset != int64(start) || wire.Log != stream[start:] {
					t.Fatalf("write %d: offset=%d want=%d; tail bytes=%d want=%d; tail matches=%v", i, wire.LogOffset, start, len(wire.Log), len(stream[start:]), wire.Log == stream[start:])
				}
				if !utf8.ValidString(j.Log) || len(j.Log) > MaxLog || wire.Log != j.Log {
					t.Fatalf("write %d: JSON changed log bytes or tail is invalid/unbounded", i)
				}
				if wire.LogOffset+int64(len(wire.Log)) != int64(len(stream)) {
					t.Fatalf("write %d: JSON byte cursor desynchronized", i)
				}
			}
		})
	}
}
