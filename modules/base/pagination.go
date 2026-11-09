package base

import (
	"strconv"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// PageRequest is the page a client asked for via ?page=&limit=.
// Crawled tables grow without bound, so list endpoints always paginate.
type PageRequest struct {
	Page  int
	Limit int
}

func ParsePage(c *fiber.Ctx) PageRequest {
	page, err := strconv.Atoi(c.Query("page"))
	if err != nil || page < 1 {
		page = 1
	}
	limit, err := strconv.Atoi(c.Query("limit"))
	if err != nil || limit < 1 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	return PageRequest{Page: page, Limit: limit}
}

func (p PageRequest) Offset() int { return (p.Page - 1) * p.Limit }

// Apply limits the query to the requested page.
func (p PageRequest) Apply(db *gorm.DB) *gorm.DB {
	return db.Offset(p.Offset()).Limit(p.Limit)
}

// Controls builds the pagination block returned to the client.
func (p PageRequest) Controls(total int64, returned int) *PaginationControls {
	pages := (total + int64(p.Limit) - 1) / int64(p.Limit)
	return &PaginationControls{
		Page:       uint(p.Page),
		Limit:      uint(p.Limit),
		Total:      uint(total),
		TotalPages: uint(pages),
		HasMore:    int64(p.Offset()+returned) < total,
	}
}
