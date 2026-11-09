package genre

import (
	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"gorm.io/gorm"
)

type GenreService struct {
	DB *gorm.DB
}

func NewService(db *gorm.DB) *GenreService {
	return &GenreService{DB: db}
}

// List returns every genre with howwe's song count and how many of those
// songs are stored locally (coverage).
func (s *GenreService) List(c *fiber.Ctx) error {
	genres := []Genre{}
	if err := s.DB.Order("site_count DESC, name").Find(&genres).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get genres")
	}
	var counts []struct {
		Genre string
		N     int64
	}
	if err := s.DB.Table("songs").Select("genre, COUNT(*) AS n").Where("genre <> ''").Group("genre").Scan(&counts).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to count songs")
	}
	byName := make(map[string]int64, len(counts))
	for _, r := range counts {
		byName[r.Genre] = r.N
	}
	for i := range genres {
		genres[i].Songs = byName[genres[i].Name]
	}
	return c.JSON(base.APIResponse{Data: genres})
}
