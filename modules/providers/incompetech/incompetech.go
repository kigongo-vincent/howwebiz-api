// Package incompetech is the Incompetech provider: Kevin MacLeod's
// royalty-free library (CC BY), ~1.4k pieces across many styles. robots.txt
// allows the music pages.
//
//	/music/royalty-free/pieces.json             the whole library, one document
//	/music/royalty-free/mp3-royaltyfree/{file}  each piece's mp3
package incompetech

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

const Name = "incompetech"

const Artist = "Kevin MacLeod"

type Site struct{ Base string } // https://incompetech.com

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: s.Base, Ingest: "push", LiveSearch: false,
		Description: "incompetech.com — Kevin MacLeod's royalty-free library (CC BY), one JSON document"}
}

func (s Site) Feeds() []string  { return []string{"pieces"} }
func (s Site) Tail(string) bool { return false }

type piece struct {
	Title    string `json:"title"`
	Filename string `json:"filename"`
	Length   string `json:"length"` // HH:MM:SS
	ISRC     string `json:"isrc"`
	Uploaded string `json:"uploaded"`
	Feel     string `json:"feel"`
}

func (s Site) FeedPage(ctx context.Context, get sitecrawl.Fetch, feed, cursor string) ([]catalog.Track, string, bool, error) {
	body, err := get(ctx, "/music/royalty-free/pieces.json")
	if err != nil {
		return nil, "", false, err
	}
	tracks, err := s.Parse(body)
	return tracks, "", true, err
}

func (s Site) Parse(body []byte) ([]catalog.Track, error) {
	var pieces []piece
	if err := json.Unmarshal(body, &pieces); err != nil {
		return nil, fmt.Errorf("incompetech: %w", err)
	}
	var out []catalog.Track
	for _, p := range pieces {
		title := strings.TrimSpace(p.Title)
		if title == "" || p.Filename == "" {
			continue
		}
		id := p.ISRC
		if id == "" {
			id = p.Filename
		}
		out = append(out, catalog.Track{Provider: Name, ExternalID: id, Title: title, Artist: Artist,
			AudioURL:   s.Base + "/music/royalty-free/mp3-royaltyfree/" + url.PathEscape(p.Filename),
			PageURL:    s.Base + "/music/royalty-free/index.html?isrc=" + url.QueryEscape(p.ISRC),
			DurationMs: durationMs(p.Length)})
	}
	return out, nil
}

func durationMs(d string) int {
	total := 0
	for _, p := range strings.Split(strings.TrimSpace(d), ":") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return total * 1000
}
