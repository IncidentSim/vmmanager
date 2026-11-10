package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type degradable struct{ degraded bool }

func (d *degradable) SetDegraded(v bool) { d.degraded = v }

// A retained/unknown durable operation must not hide itself by killing the API:
// operators and clients need the safe status endpoints, and a crash loop would
// not repair anything. Startup reports the problem and serves degraded.
func TestRecoveryFailureKeepsServingDegraded(t *testing.T) {
	var logs bytes.Buffer
	d := &degradable{}
	secret := "token=SECRET /home/x/private"
	recoverOrDegrade(func() error { return errors.New(secret) }, d, &logs)
	if !d.degraded {
		t.Fatal("API not marked degraded")
	}
	out := logs.String()
	if !strings.Contains(out, "degraded") || strings.Contains(out, "SECRET") || strings.Contains(out, "/home/x") {
		t.Fatalf("log must flag degraded and stay sanitized: %q", out)
	}
	d2 := &degradable{}
	recoverOrDegrade(func() error { return nil }, d2, &logs)
	if d2.degraded {
		t.Fatal("healthy recovery marked degraded")
	}
}
