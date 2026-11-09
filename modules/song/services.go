package song

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"gorm.io/gorm"
)

type SongService struct {
	DB *gorm.DB
}

func NewService(db *gorm.DB) *SongService {
	return &SongService{DB: db}
}

var sortOrders = map[string]string{
	"recent": "id DESC",
	"title":  "LOWER(title), id",
	"rating": "rating IS NULL, rating DESC, id DESC",
	"votes":  "votes IS NULL, votes DESC, id DESC",
}

// List returns songs, paginated.
//
//	?q=         search title or artist
//	?artistId=  filter by howwe artist id
//	?hasAudio=  true|false
//	?genre=     howwe genre name, e.g. AfroBeat
//	?sort=      recent (default) | title | rating | votes
func (s *SongService) List(c *fiber.Ctx) error {
	p := base.ParsePage(c)
	q := s.DB.Model(&Song{})

	if term := strings.TrimSpace(c.Query("q")); term != "" {
		like := "%" + strings.ToLower(term) + "%"
		q = q.Where("LOWER(title) LIKE ? OR LOWER(artist) LIKE ?", like, like)
	}
	if v := c.Query("artistId"); v != "" {
		id, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return base.API_ERROR(c, "artistId must be a number")
		}
		q = q.Where("artist_id = ?", id)
	}
	if g := c.Query("genre"); g != "" {
		q = q.Where("genre = ?", g)
	}
	if v := c.Query("hasAudio", "true"); v != "" {
		has, err := strconv.ParseBool(v)
		if err != nil {
			return base.API_ERROR(c, "hasAudio must be true or false")
		}
		if has {
			q = q.Where("audio_url <> ''")
		} else {
			q = q.Where("audio_url = ''")
		}
	}
	order, ok := sortOrders[c.Query("sort", "recent")]
	if !ok {
		return base.API_ERROR(c, "sort must be one of: recent, title, rating, votes")
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to count songs")
	}
	songs := []Song{}
	if err := p.Apply(q).Order(order).Find(&songs).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get songs")
	}
	return c.JSON(base.APIResponse{Data: songs, Pagination: p.Controls(total, len(songs))})
}

func (s *SongService) Get(c *fiber.Ctx) error {
	id, ok := base.ParamID(c, "id")
	if !ok {
		return base.API_ERROR(c, "invalid song id")
	}
	var song Song
	if err := s.DB.First(&song, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return base.API_FAIL(c, fiber.StatusNotFound, "song not found")
		}
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get song")
	}
	return c.JSON(base.APIResponse{Data: song})
}
