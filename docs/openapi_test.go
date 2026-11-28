package docs

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPISpecParses(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}

	var doc struct {
		OpenAPI string                 `yaml:"openapi"`
		Paths   map[string]interface{} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal openapi.yaml: %v", err)
	}

	if doc.OpenAPI == "" {
		t.Fatal("openapi.yaml: missing top-level 'openapi' version field")
	}

	wantPaths := []string{
		"/api/v1/jobs",
		"/api/v1/jobs/{id}",
		"/api/v1/queues",
		"/webhook/github",
		"/webhook/test",
		"/health",
	}
	for _, p := range wantPaths {
		if _, ok := doc.Paths[p]; !ok {
			t.Errorf("openapi.yaml: missing documented path %q", p)
		}
	}
}
