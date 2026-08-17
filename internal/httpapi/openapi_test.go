package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// openAPIDoc is the subset of the specification this test cares about. Path
// items are held as raw nodes because their keys are a mix of operations and
// shared fields such as `parameters`, which have different shapes.
type openAPIDoc struct {
	OpenAPI    string                          `yaml:"openapi"`
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		SecuritySchemes map[string]struct {
			Type   string `yaml:"type"`
			Scheme string `yaml:"scheme"`
		} `yaml:"securitySchemes"`
	} `yaml:"components"`
}

// operation is one documented endpoint.
type operation struct {
	OperationID string                 `yaml:"operationId"`
	Security    *[]map[string][]string `yaml:"security"`
}

// decodeOperation reads a path-item entry as an operation.
func decodeOperation(t *testing.T, node yaml.Node) operation {
	t.Helper()

	var op operation
	if err := node.Decode(&op); err != nil {
		t.Fatalf("decoding an operation failed: %v", err)
	}
	return op
}

// httpMethods are the keys of a path item that describe an operation; anything
// else (parameters, summary, ...) is not one.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

func loadOpenAPI(t *testing.T) openAPIDoc {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("reading the OpenAPI document failed: %v", err)
	}

	var doc openAPIDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the OpenAPI document is not valid YAML: %v", err)
	}
	return doc
}

// documentedOperations returns "METHOD /path" for every operation in the spec.
func documentedOperations(t *testing.T, doc openAPIDoc) []string {
	t.Helper()

	var ops []string
	for path, item := range doc.Paths {
		for method := range item {
			if slices.Contains(httpMethods, method) {
				ops = append(ops, strings.ToUpper(method)+" "+path)
			}
		}
	}
	sort.Strings(ops)
	return ops
}

// implementedOperations walks the real router. HEAD routes are excluded: they
// are an implementation detail of the probes and are not part of the documented
// API surface.
func implementedOperations(t *testing.T) []string {
	t.Helper()

	router, ok := newTestRouter(t, &stubService{}).(chi.Routes)
	if !ok {
		t.Fatal("the router does not implement chi.Routes")
	}

	var ops []string
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodHead {
			return nil
		}
		// chi renders a mounted sub-router's index as a trailing slash; the
		// specification names the collection without one.
		route = strings.TrimSuffix(route, "/")
		if route == "" {
			route = "/"
		}
		ops = append(ops, method+" "+route)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router failed: %v", err)
	}

	slices.Sort(ops)
	return slices.Compact(ops)
}

// TestOpenAPIMatchesRouter is what keeps the "OpenAPI doc matches implemented
// endpoints" requirement true over time. Without it the document is a claim;
// with it, adding a route without documenting it fails the build.
func TestOpenAPIMatchesRouter(t *testing.T) {
	t.Parallel()

	doc := loadOpenAPI(t)

	documented := documentedOperations(t, doc)
	implemented := implementedOperations(t)

	// chi renders path parameters as {name}, matching OpenAPI, so the two sets
	// are directly comparable.
	for _, op := range implemented {
		if !slices.Contains(documented, op) {
			t.Errorf("%s is implemented but not documented in api/openapi.yaml", op)
		}
	}
	for _, op := range documented {
		if !slices.Contains(implemented, op) {
			t.Errorf("%s is documented in api/openapi.yaml but not implemented", op)
		}
	}
}

func TestOpenAPIIsWellFormed(t *testing.T) {
	t.Parallel()

	doc := loadOpenAPI(t)

	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi version = %q, want 3.1.x as ROADMAP specifies", doc.OpenAPI)
	}

	scheme, ok := doc.Components.SecuritySchemes["bearerAuth"]
	if !ok {
		t.Fatal("the document declares no bearerAuth security scheme")
	}
	if scheme.Type != "http" || scheme.Scheme != "bearer" {
		t.Errorf("bearerAuth = %s/%s, want http/bearer", scheme.Type, scheme.Scheme)
	}

	// Every operation needs an operationId; generators rely on it.
	for path, item := range doc.Paths {
		for method, node := range item {
			if !slices.Contains(httpMethods, method) {
				continue
			}
			if decodeOperation(t, node).OperationID == "" {
				t.Errorf("%s %s has no operationId", strings.ToUpper(method), path)
			}
		}
	}
}

// TestHealthIsDocumentedAsUnauthenticated guards the DoD item that health
// endpoints stay exempt from the API key.
func TestHealthIsDocumentedAsUnauthenticated(t *testing.T) {
	t.Parallel()

	doc := loadOpenAPI(t)

	node, ok := doc.Paths["/healthz"]["get"]
	if !ok {
		t.Fatal("/healthz is not documented")
	}

	get := decodeOperation(t, node)
	if get.Security == nil {
		t.Fatal("/healthz does not override the global security requirement")
	}
	if len(*get.Security) != 0 {
		t.Errorf("/healthz security = %v, want an empty list meaning no auth", *get.Security)
	}
}

// TestUnauthenticatedRequestsNeverReachTheService is the runtime counterpart to
// the documented security scheme.
func TestUnauthenticatedRequestsNeverReachTheService(t *testing.T) {
	t.Parallel()

	doc := loadOpenAPI(t)

	for _, op := range documentedOperations(t, doc) {
		method, path, _ := strings.Cut(op, " ")
		if !strings.HasPrefix(path, "/v1") {
			continue
		}
		// Substitute a concrete id for the path parameter.
		target := strings.ReplaceAll(path, "{monitorID}", "11111111-2222-3333-4444-555555555555")

		svc := &stubService{}
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), method, target, nil)

		newTestRouter(t, svc).ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", op, rec.Code)
		}
		if svc.createInput != nil || svc.updateInput != nil || svc.gotLimit != 0 {
			t.Errorf("%s: the request reached the service without a credential", op)
		}
	}
}
