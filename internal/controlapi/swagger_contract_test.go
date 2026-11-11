package controlapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"gopkg.in/yaml.v2"
)

// Exercises the exact published create example against real API/SQLite owner
// admission, not a second handwritten request. Backend effects remain fake.
func TestPublishedSwaggerExampleCreatesAndReplays(t *testing.T) {
	s, _, _ := optSetup(t, Options{})
	resp, err := http.Get(s.URL + "/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var spec struct {
		Components struct {
			Schemas struct {
				CreateVM struct {
					Example struct {
						ImageID string `yaml:"image_id" json:"image_id"`
						Profile string `yaml:"profile" json:"profile"`
					} `yaml:"example"`
				} `yaml:"CreateVM"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err = yaml.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatal(err)
	}
	example := spec.Components.Schemas.CreateVM.Example
	if example.ImageID != "ubuntu" || example.Profile != "small" {
		t.Fatalf("unusable configured example: %+v", example)
	}
	b, err := json.Marshal(example)
	if err != nil {
		t.Fatal(err)
	}
	first, a := rawCall(t, s, "POST", "/v1/vms", "one", "swagger-retry", string(b))
	second, c := rawCall(t, s, "POST", "/v1/vms", "one", "swagger-retry", string(b))
	if first.StatusCode != 202 || second.StatusCode != 202 || string(a) != string(c) {
		t.Fatalf("%d %d %s %s", first.StatusCode, second.StatusCode, a, c)
	}
}
