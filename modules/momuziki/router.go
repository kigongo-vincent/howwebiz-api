package momuziki

import (
	"crypto/subtle"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
)

func RouterRegister(r *base.RouteRegister, s *Service) {
	auth := requireAPIKey(base.Env("API_KEY", ""))
	g := r.Router.Group("momuziki")
	g.Get("/status", s.Status)
	g.Post("/crawl", auth, s.Crawl)
	g.Post("/guard/reset", auth, s.ResetGuard)
	g.Get("/search", s.Search)
	g.Get("/songs", s.Songs)
	g.Get("/songs/:id", s.Song)
	g.Get("/categories", s.Categories)
	g.Get("/categories/:id/songs", s.Songs)
}

func requireAPIKey(key string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if key != "" && subtle.ConstantTimeCompare([]byte(c.Get("X-API-Key")), []byte(key)) != 1 {
			return base.API_FAIL(c, fiber.StatusUnauthorized, "missing or invalid X-API-Key")
		}
		return c.Next()
	}
}
