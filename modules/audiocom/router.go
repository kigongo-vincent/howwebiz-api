package audiocom

import (
	"crypto/subtle"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
)

func RouterRegister(r *base.RouteRegister, s *Service) {
	auth := requireAPIKey(base.Env("API_KEY", ""))
	g := r.Router.Group("audiocom")
	g.Get("/status", s.Status)
	g.Get("/connect", auth, s.Connect)
	g.Get("/callback", s.Callback) // audio.com redirects the browser here (state-checked)
	g.Post("/disconnect", auth, s.Disconnect)
	g.Post("/crawl", auth, s.Crawl)
	g.Get("/search", s.Search)
	g.Get("/tracks", s.Tracks)
	g.Get("/tracks/:id", s.Track)

	// Website (HTML) source, behind the block-counter guard.
	w := g.Group("web")
	w.Get("/status", s.WebStatus)
	w.Post("/crawl", auth, s.WebCrawl)
	w.Post("/reset", auth, s.WebReset)
	w.Get("/search", s.WebSearchHandler)
}

func requireAPIKey(key string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if key != "" && subtle.ConstantTimeCompare([]byte(c.Get("X-API-Key")), []byte(key)) != 1 {
			return base.API_FAIL(c, fiber.StatusUnauthorized, "missing or invalid X-API-Key")
		}
		return c.Next()
	}
}
