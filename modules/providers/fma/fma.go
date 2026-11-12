// Package fma is the Free Music Archive provider (freemusicarchive.org):
// Creative Commons and other free-licensed music by genre, including
// Hip-Hop, Soul-RB, Blues, Jazz, Pop, Rock, Electronic, Country, Folk.
// robots.txt allows general crawlers (it blocks named AI/SEO bots).
//
//	/genre/{Genre}/?page=N     20 tracks a page, newest first
//	/search/?quicksearch=q     live search
//
// Each track element carries JSON in data-track-info (id, title, artist,
// page and stream URLs). The stream URL redirects to the mp3 and works
// without an account.
package fma

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

const Name = "fma"

// DefaultGenres are FMA's top-level genres (/genres), minus spoken word
// and novelty.
var DefaultGenres = []string{"Hip-Hop", "Soul-RB", "Blues", "Jazz", "Pop", "Rock", "Electronic", "Country",
	"Folk", "International", "Instrumental", "Experimental", "Classical", "Old-Time__Historic"}

var genreNames = map[string]string{"Soul-RB": "Soul/R&B", "Old-Time__Historic": "Old-Time/Historic"}

type Site struct {
	Base   string   // https://freemusicarchive.org
	Genres []string // overrides DefaultGenres
}

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: s.Base, Ingest: "push", LiveSearch: true,
		Description: "freemusicarchive.org — free-licensed hip hop, soul/R&B, blues, jazz, pop, rock and more (genre pages + search)"}
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
	body, err := get(ctx, "/genre/"+url.PathEscape(genre)+"/?page="+strconv.Itoa(page))
	if err != nil {
		return nil, "", false, err
	}
	tracks, err := s.parse(body, genreName(genre))
	if err != nil {
		return nil, "", false, err
	}
	done := len(tracks) == 0 || page >= lastPage(body)
	return tracks, strconv.Itoa(page + 1), done, nil
}

func (s Site) SearchPath(q string) string { return "/search/?quicksearch=" + url.QueryEscape(q) }

func (s Site) ParseSearch(body []byte) ([]catalog.Track, error) { return s.parse(body, "") }

func genreName(g string) string {
	if n, ok := genreNames[g]; ok {
		return n
	}
	return g
}

var pageRe = regexp.MustCompile(`[?&]page=(\d+)`)

// lastPage is the highest page the pagination links to.
func lastPage(body []byte) int {
	last := 1
	for _, m := range pageRe.FindAllSubmatch(body, -1) {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n > last {
			last = n
		}
	}
	return last
}

type trackInfo struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	ArtistName  string `json:"artistName"`
	URL         string `json:"url"`
	PlaybackURL string `json:"playbackUrl"`
}

func (s Site) parse(body []byte, genre string) ([]catalog.Track, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out []catalog.Track
	seen := map[string]bool{}
	doc.Find("[data-track-info]").Each(func(_ int, el *goquery.Selection) {
		var ti trackInfo
		if json.Unmarshal([]byte(el.AttrOr("data-track-info", "")), &ti) != nil {
			return
		}
		if ti.ID == "" || seen[ti.ID] || strings.TrimSpace(ti.Title) == "" || ti.PlaybackURL == "" {
			return
		}
		seen[ti.ID] = true
		out = append(out, catalog.Track{Provider: Name, ExternalID: ti.ID, Title: strings.TrimSpace(ti.Title),
			Artist: strings.TrimSpace(ti.ArtistName), AudioURL: ti.PlaybackURL, PageURL: ti.URL, Genre: genre})
	})
	return out, nil
}
