// Package momuziki is the momuziki.com source: a PHP site crawled from its
// HTML pages (categories.php → category.php?id=N&page=P → song.php?id=N),
// plus its search page (search.php?query=…).
package momuziki

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// ErrNoSong: song.php answered with its empty shell (unknown id).
var ErrNoSong = errors.New("song page has no title or audio")

// Row is a song as listed on category and search pages.
type Row struct {
	ID       uint   `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	ThumbURL string `json:"thumbUrl"`
	AudioURL string `json:"audioUrl"`
}

type CategoryRef struct {
	ID       uint
	Name     string
	ImageURL string
}

// SongPage is what song.php adds to a listing row.
type SongPage struct {
	Title    string
	Artist   string
	ImageURL string
	AudioURL string
	Plays    *int
}

var (
	songIDRe     = regexp.MustCompile(`(?:^|/)song\.php\?id=(\d+)$`)
	categoryIDRe = regexp.MustCompile(`(?:^|/)category\.php\?id=(\d+)$`)
	playsRe      = regexp.MustCompile(`([\d,]+)\s+Plays?`)
	bgImageRe    = regexp.MustCompile(`background-image:\s*url\('([^']+)'\)`)
)

func text(s *goquery.Selection) string { return strings.Join(strings.Fields(s.Text()), " ") }

// absURL resolves href against the site root and escapes it (momuziki's
// file names contain spaces and parentheses).
func absURL(base *url.URL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	u, err := base.Parse(href)
	if err != nil {
		// Unescaped characters url.Parse rejects: escape the path by hand.
		if i := strings.Index(href, "://"); i >= 0 {
			if j := strings.Index(href[i+3:], "/"); j >= 0 {
				p := href[i+3+j:]
				if u, err = base.Parse(href[:i+3+j] + (&url.URL{Path: p}).EscapedPath()); err == nil {
					return u.String()
				}
			}
		}
		return href
	}
	return u.String()
}

func doc(body []byte) (*goquery.Document, error) {
	return goquery.NewDocumentFromReader(bytes.NewReader(body))
}

func idFrom(re *regexp.Regexp, href string) uint {
	m := re.FindStringSubmatch(strings.TrimSpace(href))
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	return uint(n)
}

// ParseCategories parses categories.php.
func ParseCategories(body []byte, base *url.URL) ([]CategoryRef, error) {
	d, err := doc(body)
	if err != nil {
		return nil, err
	}
	var out []CategoryRef
	seen := map[uint]bool{}
	d.Find("a[href*='category.php?id=']").Each(func(_ int, a *goquery.Selection) {
		id := idFrom(categoryIDRe, a.AttrOr("href", ""))
		name := text(a.Find("h3").First())
		if id == 0 || name == "" || seen[id] { // nav links have no <h3>
			return
		}
		seen[id] = true
		out = append(out, CategoryRef{ID: id, Name: name, ImageURL: absURL(base, a.Find("img").First().AttrOr("src", ""))})
	})
	if len(out) == 0 {
		return nil, errors.New("categories: none found (page layout may have changed)")
	}
	return out, nil
}

// ParseRows parses the song rows of category.php and search.php. An empty
// result is normal past the last page.
func ParseRows(body []byte, base *url.URL) ([]Row, error) {
	d, err := doc(body)
	if err != nil {
		return nil, err
	}
	var out []Row
	seen := map[uint]bool{}
	d.Find("main a[href*='song.php?id=']").Each(func(_ int, a *goquery.Selection) {
		h3 := a.Find("h3").First()
		id := idFrom(songIDRe, a.AttrOr("href", ""))
		if h3.Length() == 0 || id == 0 || seen[id] { // "Play" buttons repeat the link without a title
			return
		}
		seen[id] = true
		r := Row{ID: id, Title: text(h3), Artist: text(a.NextFiltered("p").First())}
		// The row is the nearest ancestor that also holds the download link.
		row := a.Parent()
		for i := 0; i < 4 && row.Find("a[download]").Length() == 0; i++ {
			row = row.Parent()
		}
		r.ThumbURL = absURL(base, row.Find("img").First().AttrOr("src", ""))
		r.AudioURL = absURL(base, row.Find("a[download]").First().AttrOr("href", ""))
		out = append(out, r)
	})
	return out, nil
}

// ParseSong parses song.php?id=N.
func ParseSong(body []byte, base *url.URL) (SongPage, error) {
	d, err := doc(body)
	if err != nil {
		return SongPage{}, err
	}
	main := d.Find("main").First()
	h1 := main.Find("h1").First()
	p := SongPage{
		Title:    text(h1),
		Artist:   text(h1.NextFiltered("p").First()),
		AudioURL: absURL(base, main.Find("audio[src]").First().AttrOr("src", "")),
	}
	if p.AudioURL == "" {
		p.AudioURL = absURL(base, main.Find("a[download]").First().AttrOr("href", ""))
	}
	// The banner's background image is the full-size cover. Read the style
	// attribute itself (goquery's Html() re-escapes quotes).
	if m := bgImageRe.FindStringSubmatch(main.Find("[style*='background-image']").First().AttrOr("style", "")); m != nil {
		p.ImageURL = absURL(base, m[1])
	} else if img := main.Find("img").First(); img.Length() > 0 {
		p.ImageURL = absURL(base, img.AttrOr("src", ""))
	}
	if m := playsRe.FindStringSubmatch(text(h1.Parent())); m != nil {
		if n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", "")); err == nil {
			p.Plays = &n
		}
	}
	if p.Title == "" && p.AudioURL == "" {
		return p, ErrNoSong
	}
	return p, nil
}

func pagePath(categoryID uint, page int) string {
	if page <= 1 {
		return fmt.Sprintf("/category.php?id=%d", categoryID)
	}
	return fmt.Sprintf("/category.php?id=%d&page=%d", categoryID, page)
}
