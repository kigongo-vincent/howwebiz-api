// Package sitecrawl crawls music sites that publish songs as plain HTML
// pages (the "momuziki pattern"). A provider implements only parsing for
// the capabilities its site has (and its robots.txt allows); this package
// does the crawling:
//
//	Lister     newest-first listing paginated by number:
//	           walk to its end (resumable), then each run reads from page 1
//	           until a page adds nothing new, plus a full re-walk every Refresh
//	Prober     song pages by numeric id: fetch every id not yet stored with
//	           audio, 1..highest id, then onward until ProbeAhead misses
//	Sitemapper song pages listed in a sitemap with <lastmod>: fetch pages
//	           never fetched or modified since, most recent first
//	Searchable the site's search page, used by the aggregated /songs search
//
// Everything goes through webguard (robots.txt incl. Crawl-delay, no
// cookies, block counter with circuit breaker) and lands in the catalog
// through catalog.Ingester.
package sitecrawl

import (
	"errors"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
)

// ErrNotFound: a song page exists as a URL but has no song (unknown id).
var ErrNotFound = errors.New("no song on this page")

// Site is the minimum a provider implements; capabilities are optional.
type Site interface {
	Name() string
	Info() catalog.ProviderInfo
}

// Lister: a listing ordered newest first.
type Lister interface {
	// ListPath is the path (with query) of page n (from 1).
	ListPath(page int) string
	// ParseList returns the tracks on a listing page; empty past the end.
	ParseList(body []byte) ([]catalog.Track, error)
}

// Sitemapper: song pages listed in XML sitemaps (sitemapindex or urlset).
type Sitemapper interface {
	SitemapPaths() []string
	// IsSongPage reports whether a sitemap URL path is a song page.
	IsSongPage(path string) bool
	// ParseSongPage returns the page's song first, then any it embeds.
	ParseSongPage(body []byte) ([]catalog.Track, error) // ErrNotFound if empty
}

// Prober: song pages addressable by numeric id. ParseSong may return
// several tracks (the page's own first, then any others it embeds).
type Prober interface {
	SongPath(id int) string
	ParseSong(body []byte, id int) ([]catalog.Track, error) // ErrNotFound if empty
}

// Searchable: the site has a search page.
type Searchable interface {
	SearchPath(q string) string
	ParseSearch(body []byte) ([]catalog.Track, error)
}
