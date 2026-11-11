package controlclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProfileTTLWireAndSafeFieldErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["profile"] != "small" || body["ttl_seconds"] != float64(60) || body["resources"] != nil || r.Header.Get("Idempotency-Key") != "stable-retry" {
			t.Errorf("wire: %v", body)
		}
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_failed","message":"SECRET","fields":[{"field":"ttl_seconds","code":"out_of_range","message":"outside policy"}]}}`))
	}))
	defer s.Close()
	c, err := NewUser(s.URL, "test-only", 11, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Create(context.Background(), 11, "stable-retry", CreateRequest{ImageID: "ubuntu", Profile: "small", TTLSeconds: 60})
	var api *APIError
	if !errors.As(err, &api) || api.Code != "validation_failed" || len(api.Fields) != 1 || api.Fields[0].Field != "ttl_seconds" || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error: %v", err)
	}
}
