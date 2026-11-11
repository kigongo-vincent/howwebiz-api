// Package ugmuziki is the ugmuziki.com provider: a Ugandan music
// streaming site, ~5.8k songs. robots.txt allows everything but a test path.
//
//	/sitemap.xml → /main_sitemap.xml   every /song/{slug} page with <lastmod>
//	/song/{slug}                       song page: cover, plays, and a download
//	                                   link whose file name is "Artist - Title"
//
// The page <title> is keyword-stuffed for search engines; the provider reads
// the download link instead. The crawler records the mp3 URL, never fetches it.
package ugmuziki

import (
	"bytes"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

const Name = "ugmuziki"

type Site struct{ Base string } // https://www.ugmuziki.com

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: s.Base, Ingest: "push", LiveSearch: false,
		Description: "ugmuziki.com — Ugandan music streaming (sitemap + song pages)"}
}

func (s Site) SitemapPaths() []string { return []string{"/main_sitemap.xml"} }

func (s Site) IsSongPage(path string) bool {
	rest, ok := strings.CutPrefix(path, "/song/")
	return ok && rest != "" && !strings.Contains(rest, "/")
}

var (
	onclickIDRe = regexp.MustCompile(`song_download\('(\d+)'\)`)
	playsRe     = regexp.MustCompile(`([\d,]+)\s*<small>plays</small>`)
	// greedy: split at the last " By " ("Stand By Me By X" -> "Stand By Me", "X")
	byRe = regexp.MustCompile(`(?i)^(.*)\s+by\s+(.+)$`)
)

func (s Site) ParseSongPage(body []byte) ([]catalog.Track, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	dl := doc.Find("a[download][href]").FilterFunction(func(_ int, a *goquery.Selection) bool {
		return onclickIDRe.MatchString(a.AttrOr("onclick", "")) && strings.Contains(a.AttrOr("href", ""), "/uploads/songs/")
	}).First()
	if dl.Length() == 0 {
		return nil, sitecrawl.ErrNotFound
	}
	id := onclickIDRe.FindStringSubmatch(dl.AttrOr("onclick", ""))[1]
	t := catalog.Track{Provider: Name, ExternalID: id, AudioURL: escapeURL(dl.AttrOr("href", ""))}

	// The song name reads "Title By Artist" on every page. The download file
	// name varies ("Artist - Title-ugmuziki.mp3", or, on most pages,
	// "ugmuziki.com - Title By Artist - 1000.mp3"): only a fallback.
	if m := byRe.FindStringSubmatch(strings.TrimSpace(doc.Find("#song-name" + id).First().Text())); m != nil {
		t.Title, t.Artist = strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
	} else {
		file := strings.TrimSuffix(strings.TrimSuffix(dl.AttrOr("download", ""), ".mp3"), "-ugmuziki")
		if artist, title, ok := strings.Cut(file, " - "); ok && !strings.Contains(strings.ToLower(artist), "ugmuziki") {
			t.Artist, t.Title = strings.TrimSpace(artist), strings.TrimSpace(title)
		}
	}
	if t.Title == "" {
		return nil, sitecrawl.ErrNotFound
	}
	t.CoverURL = escapeURL(doc.Find("#song-art"+id).AttrOr("src", ""))
	t.PageURL = doc.Find("#song-url"+id).AttrOr("href", "")
	if m := playsRe.FindSubmatch(body); m != nil {
		if n, err := strconv.Atoi(strings.ReplaceAll(string(m[1]), ",", "")); err == nil {
			t.Plays = &n
		}
	}
	return []catalog.Track{t}, nil
}

// escapeURL escapes the spaces the site leaves raw in file names.
func escapeURL(u string) string {
	if u == "" {
		return ""
	}
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	return p.String()
}
