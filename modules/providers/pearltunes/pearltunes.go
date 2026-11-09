// Package pearltunes is the pearltunes.com provider: Ugandan songs
// submitted by artists ("Send your music"). robots.txt allows all.
//
//	/songs/new/page/N                         newest first, 45 per page, empty past the end
//	/{Artist}/song/mp3/{id}/{slug}/{artist}  song page; resolves by id alone. It embeds
//	                                          `var pt = [...]`: the song (first) plus the
//	                                          artist's other songs, each with its mp3
//	/search?q=…                               search, same cards as the listing
package pearltunes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

const Name = "pearltunes"

type Site struct{ Base string } // e.g. https://pearltunes.com

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: s.Base, Ingest: "push", LiveSearch: true,
		Description: "pearltunes.com — artist-submitted Ugandan music (HTML pages)"}
}

func (s Site) ListPath(page int) string {
	if page <= 1 {
		return "/songs/new"
	}
	return fmt.Sprintf("/songs/new/page/%d", page)
}

func (s Site) SongPath(id int) string { return fmt.Sprintf("/x/song/mp3/%d/x/x", id) } // slugs are ignored by the site

func (s Site) SearchPath(q string) string { return "/search?" + url.Values{"q": {q}}.Encode() }

var songLinkRe = regexp.MustCompile(`/song/mp3/(\d+)/`)

// cards parses song cards (listing and search share them).
func (s Site) cards(body []byte) ([]catalog.Track, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out []catalog.Track
	seen := map[string]bool{}
	doc.Find("a[href*='/song/mp3/']").Each(func(_ int, a *goquery.Selection) {
		href := a.AttrOr("href", "")
		m := songLinkRe.FindStringSubmatch(href)
		title := strings.TrimSpace(a.Find("h3").First().Text())
		if m == nil || title == "" || seen[m[1]] {
			return
		}
		seen[m[1]] = true
		ps := a.Find("p")
		t := catalog.Track{Provider: Name, ExternalID: m[1], Title: title,
			Artist: strings.TrimSpace(ps.Eq(0).Text()), PageURL: s.abs(href),
			CoverURL: a.Find("img").First().AttrOr("src", "")}
		if ft := strings.TrimSpace(ps.Eq(1).Text()); ft != "" {
			t.Featured = splitFeat(ft)
		}
		out = append(out, t)
	})
	return out, nil
}

func splitFeat(s string) []string {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "ft."), "feat."))
	var out []string
	for _, p := range regexp.MustCompile(`\s*(?:,|&|\bx\b)\s*`).Split(s, -1) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s Site) abs(href string) string {
	if strings.HasPrefix(href, "http") {
		return href
	}
	return s.Base + "/" + strings.TrimPrefix(href, "/")
}

func (s Site) ParseList(body []byte) ([]catalog.Track, error)   { return s.cards(body) }
func (s Site) ParseSearch(body []byte) ([]catalog.Track, error) { return s.cards(body) }

var ptRe = regexp.MustCompile(`(?s)var pt = (\[.*?\]);`)

type ptItem struct {
	ID         int    `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Duration   string `json:"duration"`
	URL        string `json:"url"`
	Cover      string `json:"cover"`
	CoverLarge string `json:"cover_large"`
}

// ParseSong reads `var pt` from a song page: the page's song first (id),
// then the artist's other songs.
func (s Site) ParseSong(body []byte, id int) ([]catalog.Track, error) {
	m := ptRe.FindSubmatch(body)
	if m == nil {
		return nil, sitecrawl.ErrNotFound
	}
	var items []ptItem
	if err := json.Unmarshal(m[1], &items); err != nil {
		return nil, fmt.Errorf("pt data: %w", err)
	}
	if len(items) == 0 {
		return nil, sitecrawl.ErrNotFound
	}
	var out []catalog.Track
	for _, it := range items {
		if it.ID == 0 || it.Title == "" {
			continue
		}
		cover := it.CoverLarge
		if cover == "" {
			cover = it.Cover
		}
		out = append(out, catalog.Track{Provider: Name, ExternalID: strconv.Itoa(it.ID), Title: it.Title, Artist: it.Artist,
			AudioURL: strings.TrimSuffix(it.URL, "?_"), CoverURL: cover, DurationMs: durationMs(it.Duration),
			PageURL: fmt.Sprintf("%s/%s/song/mp3/%d/%s/%s", s.Base, slug(it.Artist, ""), it.ID, slug(it.Title, "-"), slug(it.Artist, "-"))})
	}
	if len(out) == 0 || out[0].ExternalID != strconv.Itoa(id) {
		// The page's own song must come first; anything else means the
		// layout changed or the id redirected somewhere unexpected.
		found := false
		for _, t := range out {
			found = found || t.ExternalID == strconv.Itoa(id)
		}
		if !found {
			return nil, sitecrawl.ErrNotFound
		}
	}
	return out, nil
}

var nonSlug = regexp.MustCompile(`[^A-Za-z0-9]+`)

func slug(s, sep string) string {
	out := strings.Trim(nonSlug.ReplaceAllString(s, sep), sep)
	if sep != "" {
		out = strings.ToLower(out)
	}
	if out == "" {
		return "x"
	}
	return out
}

// durationMs parses "2:59" or "1:02:03".
func durationMs(d string) int {
	parts := strings.Split(strings.TrimSpace(d), ":")
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return total * 1000
}
