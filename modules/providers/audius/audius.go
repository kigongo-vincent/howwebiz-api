// Package audius is the Audius provider (https://audius.co): an open music
// network where artists from every country upload full tracks. Its public
// REST API needs no key, only an app_name on each request.
//
//	/v1/tracks/search?query=&genre=G&sort_method=recent   newest first, per genre; offset+limit ≤ 10,000
//	/v1/tracks/search?query=q                              live search
//	/v1/tracks/{id}/stream?app_name=…                      the full track (302 to a content node, Range supported)
//
// Only tracks that anyone may stream are stored: stream-gated tracks
// (premium / follow-gated) would only give a preview.
package audius

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

const (
	Name     = "audius"
	pageSize = 100
	window   = 10000 // the search index refuses offset+limit beyond this
)

// Genres as Audius names them. Each is a newest-first feed of up to 10k
// tracks; "" is every genre together (catches new uploads in any genre).
var Genres = []string{"", "Electronic", "Rock", "Metal", "Alternative", "Hip-Hop/Rap", "Experimental", "Punk", "Folk",
	"Pop", "Ambient", "Soundtrack", "World", "Jazz", "Acoustic", "Funk", "R&B/Soul", "Devotional", "Classical", "Reggae",
	"Country", "Kids", "Latin", "Lo-Fi", "Hyperpop", "Dancehall", "Techno", "Trap", "House", "Tech House", "Deep House",
	"Disco", "Electro", "Jungle", "Progressive House", "Hardstyle", "Glitch Hop", "Trance", "Future Bass", "Future House",
	"Tropical House", "Downtempo", "Drum & Bass", "Dubstep", "Jersey Club", "Vaporwave", "Moombahton", "Afrobeats",
	"Amapiano", "Blues", "Gospel"}

type Site struct {
	Base    string // https://api.audius.co
	AppName string
	Genres  []string
	// VerifiedOnly keeps only tracks by verified artists (fewer re-uploads
	// of other people's music, but ~90% fewer tracks).
	VerifiedOnly bool
}

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: "https://audius.co", Ingest: "push", LiveSearch: true,
		Description: "audius.co — open artist-uploaded music from every country; full-length streams via the public API (no key)"}
}

func (s Site) Feeds() []string {
	out := make([]string, 0, len(s.Genres))
	for _, g := range s.Genres {
		if g == "" {
			g = "all"
		}
		out = append(out, g)
	}
	return out
}

func (s Site) Tail(string) bool { return false } // newest first

func (s Site) FeedPage(ctx context.Context, get sitecrawl.Fetch, feed, cursor string) ([]catalog.Track, string, bool, error) {
	offset, _ := strconv.Atoi(cursor)
	q := url.Values{"query": {""}, "sort_method": {"recent"}, "app_name": {s.AppName},
		"limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
	if feed != "all" {
		q.Set("genre", feed)
	}
	body, err := get(ctx, "/v1/tracks/search?"+q.Encode())
	if err != nil {
		return nil, "", false, err
	}
	raw, tracks, err := s.Parse(body)
	if err != nil {
		return nil, "", false, err
	}
	// Only an empty page ends a feed: the search index sometimes returns
	// 99 of 100 mid-feed (deduplication), so a short page is not the end.
	next := offset + pageSize
	done := raw == 0 || next+pageSize > window
	return tracks, strconv.Itoa(next), done, nil
}

func (s Site) SearchAPI(ctx context.Context, get sitecrawl.Fetch, q string, limit int) ([]catalog.Track, error) {
	v := url.Values{"query": {q}, "app_name": {s.AppName}, "limit": {strconv.Itoa(min(max(limit*2, 10), pageSize))}}
	body, err := get(ctx, "/v1/tracks/search?"+v.Encode())
	if err != nil {
		return nil, err
	}
	_, tracks, err := s.Parse(body)
	return tracks, err
}

type track struct {
	ID           string                 `json:"id"`
	Title        string                 `json:"title"`
	Genre        string                 `json:"genre"`
	Duration     int                    `json:"duration"` // seconds
	PlayCount    int                    `json:"play_count"`
	ReleaseDate  string                 `json:"release_date"`
	CreatedAt    string                 `json:"created_at"`
	UpdatedAt    string                 `json:"updated_at"`
	Permalink    string                 `json:"permalink"`
	IsStreamable bool                   `json:"is_streamable"`
	IsGated      bool                   `json:"is_stream_gated"`
	IsDelete     bool                   `json:"is_delete"`
	IsAvailable  *bool                  `json:"is_available"`
	IsUnlisted   bool                   `json:"is_unlisted"`
	Artwork      map[string]any         `json:"artwork"`
	Access       *struct{ Stream bool } `json:"access"`
	User         struct {
		Name     string `json:"name"`
		Handle   string `json:"handle"`
		Verified bool   `json:"is_verified"`
	} `json:"user"`
}

// Parse returns how many tracks the page had and those that are freely
// streamable in full.
func (s Site) Parse(body []byte) (int, []catalog.Track, error) {
	var res struct {
		Data []track `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return 0, nil, fmt.Errorf("audius: %w", err)
	}
	out := make([]catalog.Track, 0, len(res.Data))
	for _, t := range res.Data {
		if t.ID == "" || t.Title == "" || !t.IsStreamable || t.IsGated || t.IsDelete || t.IsUnlisted ||
			(t.IsAvailable != nil && !*t.IsAvailable) || (t.Access != nil && !t.Access.Stream) || (s.VerifiedOnly && !t.User.Verified) {
			continue
		}
		artist := t.User.Name
		if artist == "" {
			artist = t.User.Handle
		}
		plays := t.PlayCount
		ct := catalog.Track{Provider: Name, ExternalID: t.ID, Title: t.Title, Artist: artist, Genre: catalog.CleanGenre(t.Genre),
			AudioURL:   s.Base + "/v1/tracks/" + url.PathEscape(t.ID) + "/stream?" + url.Values{"app_name": {s.AppName}}.Encode(),
			PageURL:    "https://audius.co" + t.Permalink,
			DurationMs: t.Duration * 1000, Plays: &plays, ReleasedAt: parseTime(t.ReleaseDate)}
		for _, size := range []string{"1000x1000", "480x480", "150x150"} {
			if u, ok := t.Artwork[size].(string); ok && u != "" {
				ct.CoverURL = u
				break
			}
		}
		if ct.ReleasedAt == nil {
			ct.ReleasedAt = parseTime(t.CreatedAt)
		}
		if u := parseTime(t.UpdatedAt); u != nil {
			ct.UpdatedAt = *u
		}
		out = append(out, ct)
	}
	return len(res.Data), out, nil
}

func parseTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}
