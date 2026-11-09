// Package docs serves the OpenAPI spec (openapi.yaml, hand-written and
// kept in sync with the routes by test/docs_test.go) and a Swagger UI.
package docs

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/gofiber/fiber/v2"
	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var specYAML []byte

var (
	once     sync.Once
	specMap  map[string]any
	specJSON []byte
	specErr  error
)

func load() {
	once.Do(func() {
		var raw any
		if specErr = yaml.Unmarshal(specYAML, &raw); specErr != nil {
			specErr = fmt.Errorf("openapi.yaml: %w", specErr)
			return
		}
		m, ok := stringKeys(raw).(map[string]any)
		if !ok {
			specErr = fmt.Errorf("openapi.yaml: top level is not a mapping")
			return
		}
		specMap = m
		specJSON, specErr = json.Marshal(specMap)
	})
}

// stringKeys converts YAML mappings with non-string keys (e.g. an
// unquoted 200) into JSON-encodable map[string]any.
func stringKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			x[k] = stringKeys(c)
		}
		return x
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, c := range x {
			m[fmt.Sprint(k)] = stringKeys(c)
		}
		return m
	case []any:
		for i, c := range x {
			x[i] = stringKeys(c)
		}
		return x
	}
	return v
}

// Spec returns the parsed OpenAPI document.
func Spec() (map[string]any, error) {
	load()
	return specMap, specErr
}

// Register mounts:
//
//	GET /docs               Swagger UI
//	GET /docs/openapi.json  the spec as JSON
//	GET /docs/openapi.yaml  the spec as written
func Register(app *fiber.App) error {
	load()
	if specErr != nil {
		return specErr
	}
	app.Get("/docs", func(c *fiber.Ctx) error {
		c.Type("html")
		return c.SendString(uiHTML)
	})
	app.Get("/docs/openapi.json", func(c *fiber.Ctx) error {
		c.Type("json")
		return c.Send(specJSON)
	})
	app.Get("/docs/openapi.yaml", func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, "application/yaml")
		return c.Send(specYAML)
	})
	return nil
}

// Swagger UI from a pinned CDN version; the page only loads our own spec.
const uiHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>howwebiz-api docs</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css">
  <style>body { margin: 0; background: #fafafa; }</style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js" crossorigin></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: "/docs/openapi.json",
      dom_id: "#swagger-ui",
      deepLinking: true,
      persistAuthorization: true,
      displayRequestDuration: true,
      tryItOutEnabled: true,
      docExpansion: "list",
      filter: true
    });
  </script>
</body>
</html>`
