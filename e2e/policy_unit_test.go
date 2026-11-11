package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"gopkg.in/yaml.v2"
)

func TestAdmissionDeadlineBracket(t *testing.T) {
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	after := before.Add(500 * time.Millisecond)
	for _, tc := range []struct {
		name     string
		deadline time.Time
		wantErr  bool
	}{
		{"premature one second TTL", before.Add(time.Second), true},
		{"just before lower bound", before.Add(28*time.Second - time.Nanosecond), true},
		{"lower bound", before.Add(28 * time.Second), false},
		{"valid", before.Add(30 * time.Second), false},
		{"upper bound", after.Add(32 * time.Second), false},
		{"late", after.Add(32*time.Second + time.Nanosecond), true},
		{"same instant different timezone", before.Add(30 * time.Second).In(time.FixedZone("offset", 3600)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateAdmissionDeadline(before, after, 30*time.Second, tc.deadline); (err != nil) != tc.wantErr {
				t.Fatalf("deadline %s: err=%v, wantErr=%v", tc.deadline, err, tc.wantErr)
			}
		})
	}
}

func TestWaitGoneNotBefore(t *testing.T) {
	earliest := time.Date(2026, 1, 1, 0, 0, 28, 0, time.UTC)
	notFound := &controlclient.APIError{Status: 404}
	for _, tc := range []struct {
		name     string
		observed time.Time
		wantErr  bool
	}{
		{"premature", earliest.Add(-time.Nanosecond), true},
		{"boundary", earliest, false},
		{"after", earliest.Add(time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := waitGoneNotBefore(context.Background(), 0, earliest, func(context.Context) error {
				calls++
				return notFound
			}, func() time.Time { return tc.observed })
			if (err != nil) != tc.wantErr || calls != 1 {
				t.Fatalf("err=%v calls=%d, wantErr=%v and one observation", err, calls, tc.wantErr)
			}
		})
	}
	t.Run("present then gone", func(t *testing.T) {
		calls := 0
		err := waitGoneNotBefore(context.Background(), 0, earliest, func(context.Context) error {
			calls++
			if calls == 1 {
				return nil
			}
			return notFound
		}, func() time.Time {
			if calls == 1 {
				return earliest.Add(-time.Second)
			}
			return earliest
		})
		if err != nil || calls != 2 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})
	t.Run("bounded when never gone", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := waitGoneNotBefore(ctx, time.Hour, earliest, func(context.Context) error {
			calls++
			cancel()
			return nil
		}, func() time.Time { return earliest })
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})
	t.Run("non-404 is not disappearance", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		err := waitGoneNotBefore(ctx, time.Hour, earliest, func(context.Context) error {
			cancel()
			return &controlclient.APIError{Status: 403}
		}, func() time.Time { return earliest })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestPolicyTTLBounded(t *testing.T) {
	for _, tc := range []struct{ max, want int64 }{{86400, 600}, {30, 30}, {1, 1}} {
		got, e := policyTTL(controlclient.Capabilities{TTL: &controlclient.TTLPolicy{MaxSeconds: tc.max}}, 600)
		if e != nil || got != tc.want {
			t.Fatalf("max %d: got %d, err %v", tc.max, got, e)
		}
	}
	if _, e := policyTTL(controlclient.Capabilities{}, 600); e == nil {
		t.Fatal("missing finite policy accepted")
	}
}

func TestCPUMaxReadbackParser(t *testing.T) {
	for data, want := range map[string]float64{"50000 100000\n": 0.5, "100000 100000": 1, "150000 100000": 1.5} {
		got, e := cpuMaxRatio(data)
		if e != nil || got != want {
			t.Fatalf("quota parser: %v", e)
		}
	}
	for _, data := range []string{"max 100000", "0 100000", "50000 0", "-1 100000", "NaN 1", "50", "50 100 extra"} {
		if _, e := cpuMaxRatio(data); e == nil {
			t.Fatal("invalid/nonfinite/unlimited quota accepted")
		}
	}
}

func TestOpenAPINormalizationOnlyConfiguredExample(t *testing.T) {
	var doc map[interface{}]interface{}
	if yaml.Unmarshal([]byte("components:\n  schemas:\n    CreateVM:\n      example: {image_id: configured}\n      type: object\n    Other:\n      example: keep\n"), &doc) != nil {
		t.Fatal("fixture parse")
	}
	removeCreateExample(doc)
	schemas := doc["components"].(map[interface{}]interface{})["schemas"].(map[interface{}]interface{})
	create := schemas["CreateVM"].(map[interface{}]interface{})
	if create["example"] != nil || create["type"] != "object" || schemas["Other"].(map[interface{}]interface{})["example"] != "keep" {
		t.Fatal("normalization removed unrelated schema")
	}
}

func TestManagedEntryIdentification(t *testing.T) {
	_, pub := makeIdentity(t)
	owner, terminal := managedEntries(pub+" vibe-fc-managed:owner:mine\n"+pub+" vibe-fc-managed:terminal:mine\n"+pub+" vibe-fc-managed:owner:other\n", "mine")
	if len(owner) != 1 || len(terminal) != 1 || owner[0] != pub {
		t.Fatal("managed identity parse")
	}
}

func TestNoBootstrapSSHKeyRequired(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	if os.WriteFile(p, []byte("fixture-token"), 0600) != nil {
		t.Fatal("fixture")
	}
	_, e := loadConfig(env(map[string]string{envBaseURL: "https://example.test", envUserID: "1", envTokenF: p, envImage: "base", envDirect: "1"}))
	if e != nil {
		t.Fatal(e)
	}
}

func TestVMDeleteGuards(t *testing.T) {
	tr := &tracker{}
	tr.setBaseline([]string{"pre"}, nil)
	if e := tr.admitVM("own"); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"", "pre", "unknown"} {
		if tr.mayDeleteVM(id) == nil {
			t.Fatal("unsafe VM deletion allowed")
		}
	}
	if tr.mayDeleteVM("own") != nil {
		t.Fatal("owned VM refused")
	}
}
