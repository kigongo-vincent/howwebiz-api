// Package catalog is the aggregated song catalog: one entry per song,
// merged from every provider (howwe.ug, momuziki.com, pearltunes.com,
// ugamusic.net, audio.com, …).
//
// Layering: this package owns the domain (Track, Song, matching) and the
// use cases (ingest, index, search). Providers depend on it, never the
// other way round. They plug in through two ports:
//
//   - Store: the provider has its own store (its crawler's tables); the
//     Indexer pulls changed tracks from it incrementally.
//   - Searcher: the provider can search its site live; aggregated
//     searches fan out to every Searcher and ingest what they return.
//
// Providers without a store of their own (pearltunes, ugamusic) push
// tracks straight in with Ingest.
package catalog

import (
	"context"
	"errors"
	"time"
)

// Track is one provider's view of a song.
type Track struct {
	Provider   string
	ExternalID string // the provider's id for the song
	Title      string
	Artist     string   // as the provider shows it (may include "ft. …")
	Featured   []string // credited artists the provider lists separately
	AudioURL   string
	CoverURL   string
	PageURL    string
	DurationMs int
	Plays      *int
	Genre      string
	ReleasedAt *time.Time
	UpdatedAt  time.Time // provider-side change time (indexing watermark)
}

// Store is a provider whose tracks live in its own store.
type Store interface {
	Name() string
	// Changed returns up to limit tracks changed after the watermark
	// (updatedAt, id), ordered by (UpdatedAt, ExternalID) ascending.
	Changed(ctx context.Context, since time.Time, afterID string, limit int) ([]Track, error)
}

// Searcher is a provider that can search its site live.
type Searcher interface {
	Name() string
	Search(ctx context.Context, q string, limit int) ([]Track, error)
}

// ErrUnavailable: the searcher is not usable right now (not configured,
// not connected, paused after blocks). Reported as "skipped", not "error".
var ErrUnavailable = errors.New("provider search unavailable")
