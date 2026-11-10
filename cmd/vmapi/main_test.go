package main

import (
	"context"
	"testing"
)

func TestRequiresConfiguration(t *testing.T) {
	if e := run(context.Background(), "", openLocalBackend); e == nil {
		t.Fatal("missing configuration accepted")
	}
}
