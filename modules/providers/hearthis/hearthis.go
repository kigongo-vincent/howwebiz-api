// Package hearthis is the hearthis.at provider: a music and DJ platform
// with an open public API (api-v2.hearthis.at). Genres include hip hop,
// R&B, soul, funk, jazz, blues, pop, rock, dancehall, amapiano and many
// electronic styles; many uploads are DJ sets (the catalog files those as
// mixes by length). robots.txt disallows only download paths.
//
//	/categories/{genre}/?page=N&count=M   a genre, newest first
//	/search/?t=q&page=1&count=M           live search
package hearthis

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

const Name = "hearthis"

// DefaultGenres: hearthis category ids (/categories/).
var DefaultGenres = []string{"hiphop", "rnb", "soul", "funk", "jazz", "blues", "pop", "rock", "dancehall",
	"raggae", "amapiano", "afrotech", "lofi", "country", "folk", "indie", "songwriter", "acoustic", "house",
	"deephouse", "dance", "edm", "drumandbass", "dubstep", "techno", "trap", "disco", "classical"}

const pageSize = 20

type Site struct {
	Base   string   // https://api-v2.hearthis.at
	Genres []string // overrides DefaultGenres
}

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: "https://hearthis.at", Ingest: "push", LiveSearch: true,
		Description: "hearthis.at — music and DJ sets in every genre, open public API (hip hop, R&B, soul, funk, jazz, ...)"}
}

func (s Site) Feeds() []string {
	if len(s.Genres) > 0 {
		return s.Genres
	}
	return DefaultGenres
}

func (s Site) Tail(string) bool { return false } // newest first

// FeedPage: cursor is the page number (from 1).
func (s Site) FeedPage(ctx context.Context, get sitecrawl.Fetch, genre, cursor string) ([]catalog.Track, string, bool, error) {
	page, _ := strconv.Atoi(cursor)
	page = max(page, 1)
	body, err := get(ctx, "/categories/"+url.PathEscape(genre)+"/?page="+strconv.Itoa(page)+"&count="+strconv.Itoa(pageSize))
	if err != nil {
		return nil, "", false, err
	}
	items, err := decode(body)
	if err != nil {
		return nil, "", false, err
	}
	return tracks(items), strconv.Itoa(page + 1), len(items) == 0, nil
}

func (s Site) SearchAPI(ctx context.Context, get sitecrawl.Fetch, q string, limit int) ([]catalog.Track, error) {
	body, err := get(ctx, "/search/?"+url.Values{"t": {q}, "page": {"1"}, "count": {strconv.Itoa(min(max(limit, 1), 50))}}.Encode())
	if err != nil {
		return nil, err
	}
	items, err := decode(body)
	if err != nil {
		return nil, err
	}
	return tracks(items), nil
}

// flex is a value the API sends as a string or a number, depending on the
// endpoint ("219" or 219).
type flex string

func (f *flex) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flex(s)
		return nil
	}
	if string(b) == "null" {
		*f = ""
		return nil
	}
	*f = flex(b)
	return nil
}

type item struct {
	ID        flex   `json:"id"`
	Title     string `json:"title"`
	Duration  flex   `json:"duration"` // seconds
	Genre     string `json:"genre"`
	Stream    string `json:"stream_url"`
	Permalink string `json:"permalink_url"`
	Artwork   string `json:"artwork_url"`
	Plays     flex   `json:"playback_count"`
	User      struct {
		Username string `json:"username"`
	} `json:"user"`
}

func decode(body []byte) ([]item, error) {
	var out []item
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("hearthis: %w", err)
	}
	return out, nil
}

func tracks(items []item) []catalog.Track {
	var out []catalog.Track
	for _, it := range items {
		if it.ID == "" || strings.TrimSpace(it.Title) == "" || it.Stream == "" {
			continue
		}
		secs, _ := strconv.Atoi(string(it.Duration))
		t := catalog.Track{Provider: Name, ExternalID: string(it.ID), Title: strings.TrimSpace(it.Title),
			Artist: strings.TrimSpace(it.User.Username), Genre: it.Genre, AudioURL: it.Stream,
			PageURL: it.Permalink, CoverURL: it.Artwork, DurationMs: secs * 1000}
		if n, err := strconv.Atoi(string(it.Plays)); err == nil {
			t.Plays = &n
		}
		out = append(out, t)
	}
	return out
}
