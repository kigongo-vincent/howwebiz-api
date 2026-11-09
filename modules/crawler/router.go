package crawler

import (
	"crypto/subtle"
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
)

func RouterRegister(r *base.RouteRegister, e *Engine, sch *Scheduler) {
	s := &CrawlerService{DB: r.DB, Engine: e, Scheduler: sch}
	g := r.Router.Group("crawl")
	auth := requireAPIKey(base.Env("API_KEY", ""))
	g.Post("/", auth, s.Start)
	g.Post("/catalog", auth, s.StartCatalog)
	g.Get("/catalog", s.Catalog)
	g.Get("/status", s.Status)
	g.Get("/runs", s.Runs)
	g.Get("/runs/:id", s.Run)
}

// requireAPIKey guards crawl triggers when API_KEY is set.
func requireAPIKey(key string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if key != "" && subtle.ConstantTimeCompare([]byte(c.Get("X-API-Key")), []byte(key)) != 1 {
			return base.API_FAIL(c, fiber.StatusUnauthorized, "missing or invalid X-API-Key")
		}
		return c.Next()
	}
}

func itoa(id uint) string { return strconv.FormatUint(uint64(id), 10) }
