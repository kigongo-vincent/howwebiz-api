package config

import (
	"log/slog"

	"gorm.io/gorm"
)

// ensureIndexes adds the indexes provider tables need at scale (100k+
// rows). Each statement is idempotent and best effort: a missing index makes
// a query slower, never wrong. The catalog's own indexes live in
// catalog.Setup.
func ensureIndexes(db *gorm.DB) {
	stmts := []string{
		// The catalog indexer pages every provider store by (updated_at, id)
		// every couple of minutes; without these each pass sorts the table.
		`CREATE INDEX IF NOT EXISTS idx_songs_updated_id ON songs (updated_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_momuziki_songs_updated_id ON momuziki_songs (updated_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_audiocom_tracks_updated_id ON audiocom_tracks (updated_at, id)`,
	}
	if db.Dialector.Name() == "postgres" {
		// substring search on the per-provider list endpoints
		stmts = append(stmts,
			`CREATE EXTENSION IF NOT EXISTS pg_trgm`,
			`CREATE INDEX IF NOT EXISTS idx_momuziki_songs_title_trgm ON momuziki_songs USING gin (LOWER(title) gin_trgm_ops)`,
			`CREATE INDEX IF NOT EXISTS idx_momuziki_songs_artist_trgm ON momuziki_songs USING gin (LOWER(artist) gin_trgm_ops)`,
			`CREATE INDEX IF NOT EXISTS idx_audiocom_tracks_title_trgm ON audiocom_tracks USING gin (LOWER(title) gin_trgm_ops)`,
			`CREATE INDEX IF NOT EXISTS idx_audiocom_tracks_author_trgm ON audiocom_tracks USING gin (LOWER(author_name) gin_trgm_ops)`,
			`CREATE INDEX IF NOT EXISTS idx_audiocom_tracks_tags_trgm ON audiocom_tracks USING gin (LOWER(tags_index) gin_trgm_ops)`,
		)
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			slog.Warn("index setup skipped", "sql", s, "err", err)
		}
	}
}
