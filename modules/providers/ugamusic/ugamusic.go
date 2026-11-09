// Package ugamusic is the ugamusic.net provider. The site is a Laravel +
// Inertia app: every page embeds its data as JSON in a `data-page`
// attribute (server-rendered page data, not the /api/ robots.txt
// disallows).
//
// robots.txt disallows pagination (`/*?*page=`) and search (`/search?`),
// so this provider uses only what it allows:
//
//	/sitemap.xml → /sitemap/songs   every song page with <lastmod> (27k+)
//	/songs/{slug}                   song page: the song + related + recommended songs, each with mp3
//
// Song files live under /storage/ (also disallowed); the crawler only
// records those URLs and never fetches them. Crawl-delay: 1 is honored.
package ugamusic

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

const Name = "ugamusic"

type Site struct{ Base string } // e.g. https://ugamusic.net

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: s.Base, Ingest: "push", LiveSearch: false,
		Description: "ugamusic.net — Ugandan music (sitemap + song pages; its robots.txt disallows search and pagination)"}
}

// SitemapPaths: the songs sitemap directly (the index at /sitemap.xml
// also lists videos, blogs, albums, … which we don't need).
func (s Site) SitemapPaths() []string { return []string{"/sitemap/songs"} }

func (s Site) IsSongPage(path string) bool {
	rest, ok := strings.CutPrefix(path, "/songs/")
	return ok && rest != "" && !strings.Contains(rest, "/")
}

var dataPageRe = regexp.MustCompile(`data-page="([^"]*)"`)

type song struct {
	ID        int      `json:"id"`
	Title     string   `json:"title"`
	Slug      string   `json:"slug"`
	Artists   []string `json:"artists"`
	Audio     string   `json:"audio"`
	AudioFull string   `json:"song_url_full"`
	Cover     string   `json:"image_cover_url"`
	Artwork   string   `json:"artwork"`
	Duration  string   `json:"duration"`
	Plays     *int     `json:"plays_count"`
	Genres    string   `json:"genres"`
	Genre     *struct {
		Name string `json:"name"`
	} `json:"genre"`
	ReleaseDate string `json:"release_date"`
	CreatedAt   string `json:"created_at"`
}

// props extracts the Inertia page props.
func props(body []byte) (map[string]json.RawMessage, error) {
	m := dataPageRe.FindSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("no data-page (layout may have changed)")
	}
	var page struct {
		Props map[string]json.RawMessage `json:"props"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(string(m[1]))), &page); err != nil {
		return nil, fmt.Errorf("data-page: %w", err)
	}
	return page.Props, nil
}

// songsIn decodes a props value that is either a paginator {data: [...]}
// or a plain list.
func songsIn(raw json.RawMessage) []song {
	var pag struct {
		Data []song `json:"data"`
	}
	if json.Unmarshal(raw, &pag) == nil && pag.Data != nil {
		return pag.Data
	}
	var list []song
	_ = json.Unmarshal(raw, &list)
	return list
}

// ParseSongPage returns the page's song first, then its related and
// recommended songs.
func (s Site) ParseSongPage(body []byte) ([]catalog.Track, error) {
	p, err := props(body)
	if err != nil {
		return nil, err
	}
	var main song
	if raw, ok := p["song"]; !ok || json.Unmarshal(raw, &main) != nil || main.ID == 0 {
		return nil, sitecrawl.ErrNotFound
	}
	all := append([]song{main}, songsIn(p["related"])...)
	all = append(all, songsIn(p["recommended"])...)
	seen := map[int]bool{}
	var uniq []song
	for _, x := range all {
		if x.ID != 0 && !seen[x.ID] {
			seen[x.ID] = true
			uniq = append(uniq, x)
		}
	}
	return s.tracks(uniq), nil
}

func (s Site) abs(u string) string {
	if u == "" || strings.HasPrefix(u, "http") {
		return u
	}
	return s.Base + "/" + strings.TrimPrefix(u, "/")
}

// escapeURL escapes characters (spaces, brackets) that the site leaves
// raw in file URLs.
func escapeURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	return parsed.String()
}

func (s Site) tracks(songs []song) []catalog.Track {
	out := make([]catalog.Track, 0, len(songs))
	for _, x := range songs {
		if x.ID == 0 || x.Title == "" {
			continue
		}
		t := catalog.Track{Provider: Name, ExternalID: strconv.Itoa(x.ID), Title: x.Title, Plays: x.Plays,
			DurationMs: durationMs(x.Duration), PageURL: s.Base + "/songs/" + x.Slug}
		if len(x.Artists) > 0 {
			t.Artist, t.Featured = x.Artists[0], x.Artists[1:]
		}
		audio := x.AudioFull
		if audio == "" {
			audio = x.Audio
		}
		t.AudioURL = escapeURL(s.abs(audio))
		cover := x.Cover
		if cover == "" {
			cover = x.Artwork
		}
		t.CoverURL = escapeURL(s.abs(cover))
		switch {
		case x.Genre != nil && x.Genre.Name != "":
			t.Genre = x.Genre.Name
		case x.Genres != "":
			t.Genre = strings.TrimSpace(strings.Split(x.Genres, ",")[0])
		}
		if r, err := time.Parse(time.RFC3339Nano, x.ReleaseDate); err == nil {
			t.ReleasedAt = &r
		}
		if c, err := time.Parse(time.RFC3339Nano, x.CreatedAt); err == nil {
			t.UpdatedAt = c
		}
		out = append(out, t)
	}
	return out
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
