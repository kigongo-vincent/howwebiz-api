package song

import (
	"context"
	"strconv"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"gorm.io/gorm"
)

// CatalogStore exposes howwe.ug songs to the catalog indexer.
type CatalogStore struct{ DB *gorm.DB }

func (CatalogStore) Name() string { return "howwe" }

func (s CatalogStore) Changed(ctx context.Context, since time.Time, afterID string, limit int) ([]catalog.Track, error) {
	after, _ := strconv.ParseUint(afterID, 10, 64)
	var rows []Song
	err := s.DB.WithContext(ctx).
		Where("updated_at > ? OR (updated_at = ? AND id > ?)", since, since, after).
		Order("updated_at").Order("id").Limit(limit).Find(&rows).Error
	out := make([]catalog.Track, 0, len(rows))
	for _, r := range rows {
		out = append(out, catalog.Track{Provider: "howwe", ExternalID: strconv.FormatUint(uint64(r.ID), 10),
			Title: r.Title, Artist: r.Artist, AudioURL: r.AudioURL, CoverURL: r.CoverURL, PageURL: r.PageURL,
			Genre: r.Genre, UpdatedAt: r.UpdatedAt})
	}
	return out, err
}
