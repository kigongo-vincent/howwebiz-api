package genre

import (
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Save upserts the genre index.
func Save(db *gorm.DB, gs []scraper.Genre, now time.Time) error {
	rows := make([]Genre, len(gs))
	for i, g := range gs {
		rows[i] = Genre{Name: g.Name, Title: g.Title, Path: g.Path, SiteCount: g.SongCount, UpdatedAt: now}
	}
	return db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&rows).Error
}
