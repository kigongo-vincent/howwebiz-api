package catalog

import (
	"crypto/subtle"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
)

func RouterRegister(r *base.RouteRegister, s *Service) {
	r.Router.Get("/songs", s.List)
	r.Router.Get("/songs/:id", s.Get)
	r.Router.Get("/providers", s.ProvidersList)
	auth := requireAPIKey(base.Env("API_KEY", ""))
	r.Router.Post("/songs/reindex", auth, s.Reindex)
	r.Router.Post("/songs/cleanup", auth, s.Cleanup)
}

func requireAPIKey(key string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if key != "" && subtle.ConstantTimeCompare([]byte(c.Get("X-API-Key")), []byte(key)) != 1 {
			return base.API_FAIL(c, fiber.StatusUnauthorized, "missing or invalid X-API-Key")
		}
		return c.Next()
	}
}
