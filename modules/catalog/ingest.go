package catalog

import (
	"slices"
	"strings"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type IngestStats struct {
	Inserted  int `json:"inserted"`  // new provider sources
	Updated   int `json:"updated"`   // sources whose data changed
	Unchanged int `json:"unchanged"` // identical to what is stored → no writes
	NewSongs  int `json:"newSongs"`  // catalog entries created
	Attached  int `json:"attached"`  // new sources that joined an existing entry (merged)
	NoAudio   int `json:"noAudio"`   // rejected: no audio URL (the catalog stores playable songs only)
}

func (s *IngestStats) add(o IngestStats) {
	s.Inserted += o.Inserted
	s.Updated += o.Updated
	s.Unchanged += o.Unchanged
	s.NewSongs += o.NewSongs
	s.Attached += o.Attached
	s.NoAudio += o.NoAudio
}

// Ingester writes provider tracks into the catalog.
type Ingester struct {
	DB       *gorm.DB
	Priority []string // provider order for display fields (first wins)
}

func (in *Ingester) rank(provider string) int {
	if i := slices.Index(in.Priority, provider); i >= 0 {
		return i
	}
	return len(in.Priority)
}

func trackHash(t Track) string {
	return scraper.Fingerprint([]any{t.Title, t.Artist, t.Featured, t.AudioURL, t.CoverURL, t.PageURL,
		t.DurationMs, t.Plays, t.Genre, t.ReleasedAt})
}

// Ingest stores tracks (any providers, any size). Unchanged tracks cost
// one read; changed ones are re-matched and their songs re-aggregated.
func (in *Ingester) Ingest(tracks []Track, now time.Time) (IngestStats, error) {
	var total IngestStats
	for start := 0; start < len(tracks); start += 300 {
		st, err := in.ingestBatch(tracks[start:min(start+300, len(tracks))], now)
		total.add(st)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

type ref struct{ provider, id string }

func (in *Ingester) ingestBatch(batch []Track, now time.Time) (IngestStats, error) {
	var st IngestStats
	// Dedupe within the batch (last wins).
	byRef := map[ref]Track{}
	var order []ref
	for _, t := range batch {
		// An id too long to store cannot be matched again later: skip it.
		if t.Provider == "" || t.ExternalID == "" || len([]rune(t.ExternalID)) > externalIDSize || (t.Title == "" && t.Artist == "") {
			continue
		}
		t.Genre = CleanGenre(t.Genre)
		r := ref{t.Provider, t.ExternalID}
		if _, ok := byRef[r]; !ok {
			order = append(order, r)
		}
		byRef[r] = t
	}
	if len(order) == 0 {
		return st, nil
	}

	err := in.DB.Transaction(func(tx *gorm.DB) error {
		// Existing sources for these refs.
		existing := map[ref]Source{}
		byProvider := map[string][]string{}
		for _, r := range order {
			byProvider[r.provider] = append(byProvider[r.provider], r.id)
		}
		for p, ids := range byProvider {
			var rows []Source
			if err := tx.Where("provider = ? AND external_id IN ?", p, ids).Find(&rows).Error; err != nil {
				return err
			}
			for _, s := range rows {
				existing[ref{s.Provider, s.ExternalID}] = s
			}
		}

		// Identities and the songs they map to.
		ids := map[ref]Identity{}
		keySet := map[string]bool{}
		for _, r := range order {
			t := byRef[r]
			id := Identify(t.Title, t.Artist, t.Featured...)
			ids[r] = id
			keySet[id.Key] = true
		}
		keys := make([]string, 0, len(keySet))
		for k := range keySet {
			keys = append(keys, k)
		}
		songByKey := map[string]uint{}
		var songs []Song
		if err := tx.Select("id", "key").Where("key IN ?", keys).Find(&songs).Error; err != nil {
			return err
		}
		for _, s := range songs {
			songByKey[s.Key] = s.ID
		}

		touched := map[uint]bool{}
		for _, r := range order {
			t := byRef[r]
			old, had := existing[r]
			if had {
				t = fillFrom(t, old) // a sparser page (listing) never erases richer data (song page)
			}
			// Invariant: every stored source has an audio URL. Tracks without
			// one (listing cards, search hits) are dropped; their provider's
			// crawler fetches the song page that carries the mp3.
			if strings.TrimSpace(t.AudioURL) == "" {
				st.NoAudio++
				continue
			}
			hash := trackHash(t)
			if had && old.Hash == hash {
				st.Unchanged++
				continue
			}
			key := ids[r].Key
			songID, ok := songByKey[key]
			if !ok {
				s := Song{Key: key, Title: ids[r].Title, Artist: ids[r].Artist, FirstSeenAt: now, UpdatedAt: now}
				if err := tx.Create(&s).Error; err != nil {
					return err
				}
				songID, songByKey[key] = s.ID, s.ID
				st.NewSongs++
			} else if !had {
				st.Attached++
			}
			src := Source{SongID: songID, Provider: t.Provider, ExternalID: t.ExternalID,
				Title: t.Title, Artist: t.Artist, AudioURL: t.AudioURL, CoverURL: t.CoverURL, PageURL: t.PageURL,
				DurationMs: t.DurationMs, Plays: t.Plays, Genre: t.Genre, ReleasedAt: t.ReleasedAt,
				Hash: hash, ProviderUpdatedAt: t.UpdatedAt, UpdatedAt: now}
			if had {
				src.ID = old.ID
				if err := tx.Save(&src).Error; err != nil {
					return err
				}
				if old.SongID != songID {
					touched[old.SongID] = true // re-keyed: the old entry may be empty now
				}
				st.Updated++
			} else {
				if err := tx.Create(&src).Error; err != nil {
					return err
				}
				st.Inserted++
			}
			touched[songID] = true
		}
		return in.reaggregate(tx, touched, now)
	})
	return st, err
}

// fillFrom keeps the stored value for every field the incoming track
// leaves empty: listing pages carry less than song pages, and a re-crawl of
// a listing must not wipe the mp3 a song page provided.
func fillFrom(t Track, old Source) Track {
	if t.AudioURL == "" {
		t.AudioURL = old.AudioURL
	}
	if t.CoverURL == "" {
		t.CoverURL = old.CoverURL
	}
	if t.PageURL == "" {
		t.PageURL = old.PageURL
	}
	if t.DurationMs == 0 {
		t.DurationMs = old.DurationMs
	}
	if t.Plays == nil {
		t.Plays = old.Plays
	}
	if t.Genre == "" {
		t.Genre = old.Genre
	}
	if t.ReleasedAt == nil {
		t.ReleasedAt = old.ReleasedAt
	}
	return t
}

// reaggregate recomputes display/aggregate fields of the given songs from
// their sources, and deletes songs left without sources.
func (in *Ingester) reaggregate(tx *gorm.DB, songIDs map[uint]bool, now time.Time) error {
	if len(songIDs) == 0 {
		return nil
	}
	ids := make([]uint, 0, len(songIDs))
	for id := range songIDs {
		ids = append(ids, id)
	}
	// Bounded batches: a purge can touch tens of thousands of songs, and one
	// giant IN list (plus every source preloaded) costs memory and planner time.
	for chunk := range slices.Chunk(ids, 500) {
		if err := in.reaggregateChunk(tx, chunk, now); err != nil {
			return err
		}
	}
	return nil
}

func (in *Ingester) reaggregateChunk(tx *gorm.DB, ids []uint, now time.Time) error {
	var songs []Song
	if err := tx.Preload("Sources").Where("id IN ?", ids).Find(&songs).Error; err != nil {
		return err
	}
	for _, s := range songs {
		if len(s.Sources) == 0 {
			if err := tx.Delete(&Song{}, s.ID).Error; err != nil {
				return err
			}
			continue
		}
		srcs := s.Sources
		slices.SortStableFunc(srcs, func(a, b Source) int { return in.rank(a.Provider) - in.rank(b.Provider) })

		first := Identify(srcs[0].Title, srcs[0].Artist)
		upd := map[string]any{"title": first.Title, "artist": first.Artist, "updated_at": now,
			"genre": "", "cover_url": "", "audio_url": "", "duration_ms": 0, "plays": int64(0), "released_at": nil}
		featSeen := map[string]bool{Fold(first.Artist): true}
		var feat, providers []string
		var plays int64
		provSeen := map[string]bool{}
		for _, src := range srcs {
			id := Identify(src.Title, src.Artist)
			for _, f := range id.Featured {
				if k := Fold(f); !featSeen[k] {
					featSeen[k] = true
					feat = append(feat, f)
				}
			}
			if !provSeen[src.Provider] {
				provSeen[src.Provider] = true
				providers = append(providers, src.Provider)
			}
			if upd["genre"] == "" && src.Genre != "" {
				upd["genre"] = src.Genre
			}
			if upd["cover_url"] == "" && src.CoverURL != "" {
				upd["cover_url"] = src.CoverURL
			}
			if upd["audio_url"] == "" && src.AudioURL != "" {
				upd["audio_url"] = src.AudioURL
			}
			if upd["duration_ms"] == 0 && src.DurationMs > 0 {
				upd["duration_ms"] = src.DurationMs
			}
			if upd["released_at"] == nil && src.ReleasedAt != nil {
				upd["released_at"] = src.ReleasedAt
			}
			if src.Plays != nil {
				plays += int64(*src.Plays)
			}
		}
		if feat == nil {
			feat = []string{}
		}
		upd["plays"] = plays
		upd["featured"] = datatypes.NewJSONSlice(feat)
		upd["providers"] = datatypes.NewJSONSlice(providers)
		upd["source_count"] = len(srcs)
		upd["search_text"] = SearchText(first.Title, first.Artist, feat, s.Key)
		upd["search_version"] = SearchTextVersion
		if err := tx.Model(&Song{}).Where("id = ?", s.ID).Updates(upd).Error; err != nil {
			return err
		}
	}
	return nil
}

// Indexes keeps /songs fast on a large catalog (hundreds of thousands of
// rows): one per sort order and filter. Run with base.EnsureIndexes.
func Indexes(dialect string) []string {
	stmts := []string{
		// sort orders (each ends in id, the tie-breaker the queries use)
		`CREATE INDEX IF NOT EXISTS idx_catalog_songs_recent ON catalog_songs (first_seen_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_songs_plays ON catalog_songs (plays DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_songs_title_sort ON catalog_songs (LOWER(title), id)`,
		// ?genre= compares LOWER(genre)
		`CREATE INDEX IF NOT EXISTS idx_catalog_songs_genre_lower ON catalog_songs (LOWER(genre))`,
	}
	if dialect == "postgres" {
		// superseded by search_text (and never usable inside the old OR)
		stmts = append(stmts,
			`DROP INDEX IF EXISTS idx_catalog_songs_key_trgm`,
			`DROP INDEX IF EXISTS idx_catalog_songs_title_trgm`)
	}
	return stmts
}

// SearchIndexes is the trigram index text queries use (Postgres; needs
// pg_trgm). Build it after PrepareSearchText: filling the column first is
// far cheaper than updating a GIN index row by row.
func SearchIndexes(dialect string) []string {
	if dialect != "postgres" {
		return nil
	}
	return []string{`CREATE INDEX IF NOT EXISTS idx_catalog_songs_search_trgm ON catalog_songs USING gin (search_text gin_trgm_ops)`}
}

type PurgeStats struct {
	SourcesRemoved int64 `json:"sourcesRemoved"`
	SongsRemoved   int64 `json:"songsRemoved"`
	SongsUpdated   int   `json:"songsUpdated"`
}

// PurgeWithoutAudio enforces the audio-only invariant on existing data:
// it deletes sources without an audio URL, re-aggregates the songs they
// belonged to, and deletes songs left without a source (or without audio).
// Idempotent; cheap when there is nothing to do.
func (in *Ingester) PurgeWithoutAudio(now time.Time) (PurgeStats, error) {
	var st PurgeStats
	// usually there's nothing to do (new data without audio is never stored):
	// find that out with two cheap lookups that stop at the first hit, rather
	// than counting the whole catalog and holding a transaction open over it
	// on every start
	var work bool
	if err := in.DB.Raw(`SELECT EXISTS (SELECT 1 FROM catalog_sources WHERE TRIM(audio_url) = '')
		OR EXISTS (SELECT 1 FROM catalog_songs WHERE TRIM(audio_url) = '')`).Scan(&work).Error; err != nil {
		return st, err
	}
	if !work {
		return st, nil
	}
	err := in.DB.Transaction(func(tx *gorm.DB) error {
		var before, after int64
		if err := tx.Model(&Song{}).Count(&before).Error; err != nil {
			return err
		}
		var songIDs []uint
		if err := tx.Model(&Source{}).Where("TRIM(audio_url) = ''").Distinct().Pluck("song_id", &songIDs).Error; err != nil {
			return err
		}
		res := tx.Where("TRIM(audio_url) = ''").Delete(&Source{})
		if res.Error != nil {
			return res.Error
		}
		st.SourcesRemoved = res.RowsAffected
		// Songs that lost sources, plus songs whose aggregate still says "no
		// audio" (left over from before the invariant): re-aggregate them
		// from the sources they still have.
		var stale []uint
		if err := tx.Model(&Song{}).Where("TRIM(audio_url) = ''").Pluck("id", &stale).Error; err != nil {
			return err
		}
		touched := make(map[uint]bool, len(songIDs)+len(stale))
		for _, id := range append(songIDs, stale...) {
			touched[id] = true
		}
		if err := in.reaggregate(tx, touched, now); err != nil {
			return err
		}
		st.SongsUpdated = len(touched)
		// NOT EXISTS uses the song_id index; NOT IN (subquery) can't.
		if err := tx.Where("NOT EXISTS (SELECT 1 FROM catalog_sources WHERE catalog_sources.song_id = catalog_songs.id)").Delete(&Song{}).Error; err != nil {
			return err
		}
		if err := tx.Model(&Song{}).Count(&after).Error; err != nil {
			return err
		}
		st.SongsRemoved = before - after
		return nil
	})
	return st, err
}
