// Package mdundo is the Mdundo provider (play.mdundo.com): a licensed
// African music service (artists are paid per play/download) with a large
// Ugandan catalog. robots.txt allows everything.
//
// Pages render their song lists server side; every song is a
// .md-playlist-song-item element whose data-* attributes carry the id,
// title, artist, duration, cover and a stable stream URL
// (https://mdundo.com/stream/{id}, which redirects to a signed mp3).
// Pages are not paginated (?page= returns the same list), so the provider
// reads many lists, each a one-page feed:
//
//	/top-charts/{country}   charts (Uganda first)
//	/genre/{id}/{name}      top songs of each genre (DJ-Mixes are mixes)
//
// and answers live searches with /search-results?term=.
// Premium tracks (behind Mdundo's subscription) are skipped.
package mdundo

import (
	"bytes"
	"context"
	"html"
	"net/url"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

const Name = "mdundo"

// DefaultFeeds: East African charts first, then every genre page listed at
// /genres. Override with MDUNDO_FEEDS (comma-separated paths).
var DefaultFeeds = []string{
	"top-charts/ug", "top-charts/ke", "top-charts/tz", "top-charts/rw", "top-charts/global",
	"genre/1/Gospel", "genre/155/Afrobeats", "genre/3/Dancehall", "genre/2/Reggae", "genre/114/Pop",
	"genre/143/Hip-Hop-Rap", "genre/241/R-B-Soul", "genre/253/Traditional", "genre/250/Afro-Vernacular",
	"genre/177/African-Fusion", "genre/246/Bongo-Flava", "genre/282/Amapiano", "genre/261/Zouk",
	"genre/324/Highlife", "genre/9/Rhumba-Zilizopendwa", "genre/258/Sebene", "genre/11/Benga",
	"genre/8/Taarab", "genre/288/Singeli", "genre/6/Kapuka", "genre/7/Genge", "genre/308/Instrumentals",
	"genre/245/DJ-Mixes",
}

type Site struct {
	Base string   // https://play.mdundo.com
	Feed []string // overrides DefaultFeeds
}

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: s.Base, Ingest: "push", LiveSearch: true,
		Description: "mdundo.com — licensed African music (artists paid per play); Ugandan and East African charts, every genre, live search"}
}

// Feeds: one-page lists (charts and genres).
func (s Site) Feeds() []string {
	if len(s.Feed) > 0 {
		return s.Feed
	}
	return DefaultFeeds
}

func (s Site) Tail(string) bool { return false }

func (s Site) FeedPage(ctx context.Context, get sitecrawl.Fetch, feed, cursor string) ([]catalog.Track, string, bool, error) {
	body, err := get(ctx, "/"+strings.TrimPrefix(feed, "/"))
	if err != nil {
		return nil, "", false, err
	}
	tracks, err := s.parseCards(body, "/"+strings.TrimPrefix(feed, "/"))
	return tracks, "", true, err // a list is one page
}

// genreOf names the genre of a "genre/{id}/{Name}" feed.
func genreOf(feed string) string {
	parts := strings.Split(feed, "/")
	if len(parts) == 3 && parts[0] == "genre" {
		return strings.ReplaceAll(parts[2], "-", " ")
	}
	return ""
}

func (s Site) SearchPath(q string) string { return "/search-results?term=" + url.QueryEscape(q) }

func (s Site) ParseSearch(body []byte) ([]catalog.Track, error) { return s.parseCards(body, "") }

// parseCards reads every song element on a page (each appears once; the
// same data also sits escaped in a script, which goquery doesn't see).
// Pages also show other lists (the chart widget is on every page): only
// the cards of list (the page's own, by data-playlist-url) get its genre.
func (s Site) parseCards(body []byte, list string) ([]catalog.Track, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out []catalog.Track
	seen := map[string]bool{}
	doc.Find(".md-playlist-song-item[data-id][data-url-play]").Each(func(_ int, el *goquery.Selection) {
		attr := func(k string) string { return strings.TrimSpace(el.AttrOr("data-"+k, "")) }
		id := attr("id")
		if id == "" || seen[id] || attr("is-premium") == "1" {
			return
		}
		seen[id] = true
		artist := clean(attr("artist"))
		title := stripBy(clean(attr("title")), artist)
		if title == "" {
			return
		}
		secs, _ := strconv.Atoi(attr("duration-seconds"))
		genre := ""
		if list != "" && attr("playlist-url") == list {
			genre = genreOf(strings.TrimPrefix(list, "/"))
		}
		t := catalog.Track{Provider: Name, ExternalID: id, Title: title, Artist: artist,
			AudioURL: attr("url-play"), CoverURL: attr("image"), DurationMs: secs * 1000, Genre: genre,
			PageURL: s.Base + attr("artist-url") + "#autoplay-" + id}
		if attr("is-dj-mix-playlist") == "1" { // Mdundo's own flag; IsMix covers the rest
			t.Kind = catalog.KindMix
		}
		out = append(out, t)
	})
	return out, nil
}

// clean undoes the double escaping some titles carry ("&amp;amp;").
func clean(s string) string {
	for range 2 {
		if !strings.Contains(s, "&") {
			break
		}
		s = html.UnescapeString(s)
	}
	return strings.TrimSpace(s)
}

// stripBy drops the " By {artist}" many uploads append to the title
// ("Balippila Boda By Eddy Kenzo").
func stripBy(title, artist string) string {
	if artist == "" {
		return title
	}
	suffix := " by " + strings.ToLower(artist)
	if strings.HasSuffix(strings.ToLower(title), suffix) {
		return strings.TrimSpace(title[:len(title)-len(suffix)])
	}
	return title
}
