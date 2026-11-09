package test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/config"
	"github.com/kigongo-vincent/howwebiz-api/modules/docs"
)

var routeParamRe = regexp.MustCompile(`:([A-Za-z_]+)`)

// TestDocsMatchRoutes fails when a route is added, removed or renamed
// without updating modules/docs/openapi.yaml (and vice versa).
func TestDocsMatchRoutes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := config.ConnectDB()
	app := config.SetupApplication()
	config.RegisterAppRoutes(config.Wire(ctx, db, app, false))

	routes := map[string]bool{}
	for _, r := range app.GetRoutes(true) {
		if r.Method == http.MethodHead || r.Method == "USE" || !strings.HasPrefix(r.Path, "/api/") {
			continue
		}
		p := routeParamRe.ReplaceAllString(strings.TrimSuffix(r.Path, "/"), "{$1}")
		routes[r.Method+" "+p] = true
	}

	spec, err := docs.Spec()
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for path, item := range spec["paths"].(map[string]any) {
		for method, op := range item.(map[string]any) {
			key := strings.ToUpper(method) + " " + path
			documented[key] = true
			// every {param} in the path is declared as a path parameter
			for _, m := range regexp.MustCompile(`\{(\w+)\}`).FindAllStringSubmatch(path, -1) {
				found := false
				params, _ := op.(map[string]any)["parameters"].([]any)
				for _, p := range params {
					if pm, ok := p.(map[string]any); ok && pm["name"] == m[1] && pm["in"] == "path" {
						found = true
					}
				}
				if !found {
					t.Errorf("%s: path parameter {%s} not declared", key, m[1])
				}
			}
		}
	}

	var missing, stale []string
	for k := range routes {
		if !documented[k] {
			missing = append(missing, k)
		}
	}
	for k := range documented {
		if !routes[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("routes missing from openapi.yaml:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("openapi.yaml documents routes that don't exist:\n  %s", strings.Join(stale, "\n  "))
	}
	if len(routes) < 30 {
		t.Fatalf("only %d routes found; is the app fully wired?", len(routes))
	}

	// every $ref resolves
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
				var cur any = spec
				for _, p := range parts {
					m, ok := cur.(map[string]any)
					if !ok || m[p] == nil {
						t.Errorf("unresolved $ref %s", ref)
						return
					}
					cur = m[p]
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(spec)

	// served endpoints
	res, _ := app.Test(httptest.NewRequest(http.MethodGet, "/docs", nil))
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(body), "/docs/openapi.json") || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("/docs: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res, _ = app.Test(httptest.NewRequest(http.MethodGet, "/docs/openapi.json", nil))
	var served map[string]any
	if err := json.NewDecoder(res.Body).Decode(&served); err != nil || served["openapi"] != "3.0.3" {
		t.Fatalf("/docs/openapi.json: %v %v", err, served["openapi"])
	}
	res, _ = app.Test(httptest.NewRequest(http.MethodGet, "/docs/openapi.yaml", nil))
	if res.StatusCode != 200 {
		t.Fatalf("/docs/openapi.yaml: %d", res.StatusCode)
	}
	t.Logf("%d routes documented", len(routes))
}
