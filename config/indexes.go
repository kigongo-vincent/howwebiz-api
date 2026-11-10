package config

import (
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/search"
)

// appIndexes lists every index the app needs at scale (100k+ rows), except
// the catalog's search_text index (catalog.SearchIndexes), which is built
// after that column is filled. They are built in the background after the
// API is listening (base.EnsureIndexes), never on the boot path.
func appIndexes(dialect string) []string {
	stmts := append(search.Indexes(dialect), catalog.Indexes(dialect)...)
	stmts = append(stmts,
		// The catalog indexer pages every provider store by (updated_at, id)
		// every couple of minutes; without these each pass sorts the table.
		`CREATE INDEX IF NOT EXISTS idx_songs_updated_id ON songs (updated_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_momuziki_songs_updated_id ON momuziki_songs (updated_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_audiocom_tracks_updated_id ON audiocom_tracks (updated_at, id)`,
	)
	if dialect == "postgres" {
		// substring search on the per-provider list endpoints
		stmts = append(stmts,
			`CREATE INDEX IF NOT EXISTS idx_momuziki_songs_title_trgm ON momuziki_songs USING gin (LOWER(title) gin_trgm_ops)`,
			`CREATE INDEX IF NOT EXISTS idx_momuziki_songs_artist_trgm ON momuziki_songs USING gin (LOWER(artist) gin_trgm_ops)`,
			`CREATE INDEX IF NOT EXISTS idx_audiocom_tracks_title_trgm ON audiocom_tracks USING gin (LOWER(title) gin_trgm_ops)`,
			`CREATE INDEX IF NOT EXISTS idx_audiocom_tracks_author_trgm ON audiocom_tracks USING gin (LOWER(author_name) gin_trgm_ops)`,
			`CREATE INDEX IF NOT EXISTS idx_audiocom_tracks_tags_trgm ON audiocom_tracks USING gin (LOWER(tags_index) gin_trgm_ops)`,
		)
	}
	return stmts
}
