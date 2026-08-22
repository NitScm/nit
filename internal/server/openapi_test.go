package server_test

import (
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NitScm/nit/api"
	"github.com/NitScm/nit/internal/server"
)

// spec is the part of the OpenAPI document these tests reason about.
type spec struct {
	OpenAPI string `yaml:"openapi"`
	Info    struct {
		Title   string `yaml:"title"`
		Version string `yaml:"version"`
	} `yaml:"info"`
	Paths      map[string]map[string]operation `yaml:"paths"`
	Components struct {
		Schemas   map[string]any `yaml:"schemas"`
		Responses map[string]any `yaml:"responses"`
	} `yaml:"components"`
}

type operation struct {
	OperationID string           `yaml:"operationId"`
	Summary     string           `yaml:"summary"`
	Tags        []string         `yaml:"tags"`
	Responses   map[string]any   `yaml:"responses"`
	Security    []map[string]any `yaml:"security"`
}

func parseSpec(t *testing.T) spec {
	t.Helper()

	var parsed spec
	if err := yaml.Unmarshal(api.OpenAPI, &parsed); err != nil {
		t.Fatalf("the embedded specification is not valid YAML: %v", err)
	}

	return parsed
}

func TestSpecIsWellFormed(t *testing.T) {
	parsed := parseSpec(t)

	if !strings.HasPrefix(parsed.OpenAPI, "3.") {
		t.Errorf("openapi = %q", parsed.OpenAPI)
	}
	if parsed.Info.Title == "" || parsed.Info.Version == "" {
		t.Error("info.title and info.version are required")
	}
	if len(parsed.Paths) == 0 {
		t.Fatal("the specification describes no path")
	}
}

// The test that keeps the description honest. A specification that drifts from
// its implementation is worse than none, because it is believed.
func TestSpecCoversEveryRoute(t *testing.T) {
	f := newFixture(t)
	parsed := parseSpec(t)

	described := map[string]bool{}
	for path, operations := range parsed.Paths {
		for method := range operations {
			described[strings.ToUpper(method)+" "+path] = true
		}
	}

	var missing []string

	for _, route := range f.server.Routes() {
		// Go's ServeMux writes wildcards as {id}; so does OpenAPI.
		if !described[route] {
			missing = append(missing, route)
		}
	}

	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("routes served but not described in api/openapi.yaml:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// And the other direction: a documented endpoint that does not exist sends
// clients somewhere there is nothing.
func TestSpecDescribesNothingExtra(t *testing.T) {
	f := newFixture(t)
	parsed := parseSpec(t)

	served := map[string]bool{}
	for _, route := range f.server.Routes() {
		served[route] = true
	}

	var extra []string

	for path, operations := range parsed.Paths {
		for method := range operations {
			route := strings.ToUpper(method) + " " + path
			if !served[route] {
				extra = append(extra, route)
			}
		}
	}

	sort.Strings(extra)

	if len(extra) > 0 {
		t.Errorf("described in api/openapi.yaml but not served:\n  %s",
			strings.Join(extra, "\n  "))
	}
}

// Every operation needs an id and a summary: they are what a generated client
// names its methods after, and what a reader scans.
func TestEveryOperationIsIdentified(t *testing.T) {
	parsed := parseSpec(t)

	seen := map[string]string{}

	for path, operations := range parsed.Paths {
		for method, op := range operations {
			where := strings.ToUpper(method) + " " + path

			if op.OperationID == "" {
				t.Errorf("%s has no operationId", where)
				continue
			}
			if op.Summary == "" {
				t.Errorf("%s has no summary", where)
			}
			if len(op.Tags) == 0 {
				t.Errorf("%s has no tag", where)
			}
			if len(op.Responses) == 0 {
				t.Errorf("%s documents no response", where)
			}

			if previous, ok := seen[op.OperationID]; ok {
				t.Errorf("operationId %q is used by both %s and %s",
					op.OperationID, previous, where)
			}
			seen[op.OperationID] = where
		}
	}
}

// An authenticated endpoint that does not document 401 leaves a client author
// guessing at the one failure they will certainly meet.
func TestAuthenticatedOperationsDocumentUnauthorized(t *testing.T) {
	parsed := parseSpec(t)

	for path, operations := range parsed.Paths {
		for method, op := range operations {
			// security: [] marks the two unauthenticated endpoints.
			if op.Security != nil && len(op.Security) == 0 {
				continue
			}
			if strings.HasPrefix(path, "/v1/admin/") {
				// The operations API answers 404 to a caller who is not an
				// operator, so that is what it documents.
				if _, ok := op.Responses["404"]; !ok {
					t.Errorf("%s %s does not document 404", strings.ToUpper(method), path)
				}
				continue
			}

			if _, ok := op.Responses["401"]; !ok {
				t.Errorf("%s %s does not document 401", strings.ToUpper(method), path)
			}
		}
	}
}

// Every error code the server can emit must be in the documented enum, or a
// client branching on `code` meets one it has never heard of.
func TestErrorCodesAreDocumented(t *testing.T) {
	var document struct {
		Components struct {
			Schemas struct {
				Error struct {
					Properties struct {
						Code struct {
							Enum []string `yaml:"enum"`
						} `yaml:"code"`
					} `yaml:"properties"`
				} `yaml:"Error"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}

	if err := yaml.Unmarshal(api.OpenAPI, &document); err != nil {
		t.Fatalf("parse: %v", err)
	}

	documented := map[string]bool{}
	for _, code := range document.Components.Schemas.Error.Properties.Code.Enum {
		documented[code] = true
	}

	// The codes the protocol package defines, plus the ones the HTTP layer
	// produces for authentication and request problems.
	for _, code := range []string{
		"unauthorized_paths", "stale_sync_point", "unknown_sync_point",
		"branch_busy", "patch_too_large", "unsupported_version",
		"unknown_repository", "conflict",
		"bad_request", "no_credentials", "malformed_credentials", "invalid_token",
		"token_expired", "token_revoked", "user_disabled", "user_not_in_policy",
		"unknown_workspace", "unknown_task", "unknown_blob", "digest_mismatch",
		"task_not_ready", "patch_expired", "not_found", "internal",
	} {
		if !documented[code] {
			t.Errorf("error code %q is not in the documented enum", code)
		}
	}
}

// The description has to be reachable before a client has a credential: it is
// what says how to obtain one.
func TestOpenAPIIsServedUnauthenticated(t *testing.T) {
	f := newFixture(t)

	resp := f.do(http.MethodGet, server.RouteOpenAPI, "", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "yaml") {
		t.Errorf("Content-Type = %q", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if len(body) != len(api.OpenAPI) {
		t.Errorf("served %d bytes, embedded is %d", len(body), len(api.OpenAPI))
	}

	// It must describe the API, not the deployment: no hostnames, no policy, no
	// identities beyond the illustrative ones.
	if strings.Contains(string(body), "postgres://") {
		t.Error("the specification leaks a database URL")
	}
}
