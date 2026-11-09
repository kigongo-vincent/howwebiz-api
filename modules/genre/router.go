package genre

import (
	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/song"
)

func RouterRegister(r *base.RouteRegister) {
	s := NewService(r.DB)
	songs := song.NewService(r.DB)
	g := r.Router.Group("genres")
	g.Get("/", s.List)
	// Same as /songs?genre=:name (all /songs filters and sorting apply).
	g.Get("/:name/songs", func(c *fiber.Ctx) error {
		name := c.Params("name")
		var n int64
		if err := r.DB.Model(&Genre{}).Where("name = ?", name).Count(&n).Error; err != nil {
			return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get genre")
		}
		if n == 0 {
			return base.API_FAIL(c, fiber.StatusNotFound, "unknown genre")
		}
		c.Request().URI().QueryArgs().Set("genre", name)
		return songs.List(c)
	})
}
