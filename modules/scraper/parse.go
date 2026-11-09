package scraper

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

// SongRef is a song as it appears on a listing/chart page.
type SongRef struct {
	ID         int64  `json:"id"`
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	ArtistID   int64  `json:"artist_id"`
	ArtistSlug string `json:"artist_slug"`
	PagePath   string `json:"page_path"`
	CoverURL   string `json:"cover_url"`
	Genre      string `json:"genre,omitempty"` // set when seen on a genre page
}

// Genre is one entry of the /genres index.
type Genre struct {
	Name      string // URL name, e.g. "AfroBeat"
	Title     string // display name
	Path      string // "/genres/AfroBeat"
	SongCount int    // as reported by the site
}

// ChartEntry is one row of the Hot 100 chart.
type ChartEntry struct {
	Position     int     `json:"position"`
	Trend        string  `json:"trend"` // up | down | steady | new
	LastDay      *int    `json:"last_day"`
	LastWeek     *int    `json:"last_week"`
	Peak         *int    `json:"peak"`
	WeeksOnChart *int    `json:"weeks_on_chart"`
	Song         SongRef `json:"song"`
}

// ListingPage is one page of an /audio/* listing.
type ListingPage struct {
	Songs   []SongRef
	HasNext bool
}

// SongDetail holds the fields only available on a song's own page.
type SongDetail struct {
	Title     string
	Artist    string
	ArtistURL string
	AudioURL  string
	FileSize  string
	AddedDate string
	Album     string
	AlbumURL  string
	VideoURL  string
	Rating    *float64
	Votes     *int

	// Canonical is the song's real /song/{id}/{slug}/{artistId}/{artistSlug}
	// path when the page links to it (zero if not found). Lets a song that
	// was reached by id alone be stored with its proper slug and artist id.
	Canonical SongRef `json:"-"`
}

// ProbePath is a song URL that howwe.ug resolves by id alone; the slug
// segments are ignored by the site.
func ProbePath(id int64) string { return fmt.Sprintf("/song/%d/x/1/x", id) }

// ParseSongPath extracts the ids and slugs from a /song/... link.
func ParseSongPath(href string) (SongRef, bool) { return parseSongPath(href) }

// ErrNoSong: the page is howwe.ug's empty shell for an id with no song
// (deleted or never used), or the layout changed beyond recognition.
var ErrNoSong = errors.New("song page has no title or audio")

var songPathRe = regexp.MustCompile(`^/song/(\d+)/([^/]*)/(\d+)/([^/?#]*)`)

func parseSongPath(href string) (SongRef, bool) {
	if u, err := url.Parse(href); err == nil && u.Path != "" {
		href = u.Path
	}
	m := songPathRe.FindStringSubmatch(href)
	if m == nil {
		return SongRef{}, false
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	aid, _ := strconv.ParseInt(m[3], 10, 64)
	return SongRef{ID: id, Slug: m[2], ArtistID: aid, ArtistSlug: m[4], PagePath: m[0]}, true
}

func text(s *goquery.Selection) string {
	return strings.Join(strings.Fields(s.Text()), " ")
}

// ParseHot100 parses the /hot100 chart page.
func ParseHot100(body []byte) ([]ChartEntry, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out []ChartEntry
	doc.Find("li[id]").Each(func(_ int, li *goquery.Selection) {
		a := li.ChildrenFiltered("a[href^='/song/']").First()
		href, _ := a.Attr("href")
		ref, ok := parseSongPath(href)
		if !ok {
			return
		}
		cols := a.ChildrenFiltered("div")
		pos, err := strconv.Atoi(text(cols.Eq(0).ChildrenFiltered("span").First()))
		if err != nil {
			return
		}
		info := a.Find("div.flex-auto").First().ChildrenFiltered("span")
		ref.Title = text(info.Eq(0))
		ref.Artist = text(info.Eq(1))
		ref.CoverURL, _ = a.Find("img").First().Attr("src")

		e := ChartEntry{Position: pos, Song: ref, Trend: trendOf(cols.Eq(0).Find("div.transform").AttrOr("class", ""))}
		cols.Each(func(_ int, col *goquery.Selection) {
			label := col.ChildrenFiltered("span.text-2xs")
			if label.Length() == 0 {
				return
			}
			name := text(label)
			val := strings.TrimSpace(strings.TrimSuffix(text(col), name))
			n := intPtr(val)
			switch name {
			case "LD":
				e.LastDay = n
			case "LW":
				e.LastWeek = n
			case "PV":
				e.Peak = n
			case "WKs":
				e.WeeksOnChart = n
			}
		})
		out = append(out, e)
	})
	if len(out) == 0 {
		return nil, fmt.Errorf("hot100: no chart entries found (page layout may have changed)")
	}
	return out, nil
}

func trendOf(class string) string {
	switch {
	case strings.Contains(class, "text-green"):
		return "up"
	case strings.Contains(class, "text-red"):
		return "down"
	case strings.Contains(class, "text-gray"):
		return "steady"
	case strings.Contains(class, "text-yellow"):
		return "new"
	}
	return ""
}

func intPtr(s string) *int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &n
}

// ParseListing parses a paginated song grid: /audio/{list} or
// /genres/{name}, optionally followed by /page/N. listPath is the page-1
// path, e.g. "/audio/new" or "/genres/Acapella".
func ParseListing(body []byte, listPath string, page int) (ListingPage, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return ListingPage{}, err
	}
	var lp ListingPage
	doc.Find("ul.grid > li > a[href^='/song/']").Each(func(_ int, a *goquery.Selection) {
		ref, ok := parseSongPath(a.AttrOr("href", ""))
		if !ok {
			return
		}
		ref.Title = text(a.Find("h3").First())
		ref.Artist = text(a.Find("h4").First())
		ref.CoverURL = a.Find("img").First().AttrOr("src", "")
		lp.Songs = append(lp.Songs, ref)
	})
	next := fmt.Sprintf("%s/page/%d", listPath, page+1)
	doc.Find("a[href]").EachWithBreak(func(_ int, a *goquery.Selection) bool {
		if strings.HasSuffix(a.AttrOr("href", ""), next) {
			lp.HasNext = true
			return false
		}
		return true
	})
	return lp, nil
}

var songCountRe = regexp.MustCompile(`([\d,]+)\s+songs?`)

// ParseGenres parses the /genres index.
func ParseGenres(body []byte) ([]Genre, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out []Genre
	seen := map[string]bool{}
	doc.Find("a[href]").Each(func(_ int, a *goquery.Selection) {
		href := strings.TrimPrefix(a.AttrOr("href", ""), "/")
		name, ok := strings.CutPrefix(href, "genres/")
		if !ok || name == "" || strings.Contains(name, "/") || seen[name] {
			return
		}
		g := Genre{Name: name, Title: text(a.Find("h3").First()), Path: "/genres/" + name}
		if g.Title == "" {
			return // nav links etc.; genre cards have a title + count
		}
		if m := songCountRe.FindStringSubmatch(text(a.Find("h4").First())); m != nil {
			g.SongCount, _ = strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
		}
		seen[name] = true
		out = append(out, g)
	})
	if len(out) == 0 {
		return nil, errors.New("genres: none found (page layout may have changed)")
	}
	return out, nil
}

var (
	ratingRe = regexp.MustCompile(`([\d.]+) of 5 stars`)
	votesRe  = regexp.MustCompile(`([\d,]+) votes`)
)

// ParseSong parses a /song/... detail page. pageURL is used to resolve
// relative links when the page has no <base>.
func ParseSong(body []byte, pageURL string) (SongDetail, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return SongDetail{}, err
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return SongDetail{}, err
	}
	pagePath := base.Path
	if b, ok := doc.Find("base[href]").Attr("href"); ok {
		if bu, err := base.Parse(b); err == nil {
			if bu.Path == "" {
				bu.Path = "/"
			}
			base = bu
		}
	}
	abs := func(href string) string {
		if href == "" {
			return ""
		}
		u, err := base.Parse(href)
		if err != nil {
			return href
		}
		return u.String()
	}

	d := SongDetail{Title: text(doc.Find("h1").First())}

	doc.Find("label").Each(func(_ int, l *goquery.Selection) {
		val := text(l.NextFiltered("span"))
		link := l.Parent().Filter("a").AttrOr("href", "")
		switch strings.TrimSuffix(text(l), ":") {
		case "Name":
			d.Artist, d.ArtistURL = val, abs(link)
		case "Added Date":
			d.AddedDate = val
		case "Album":
			d.Album, d.AlbumURL = val, abs(link)
		}
	})

	// The first playlist item is the song being viewed; the rest are
	// other songs by the same artist.
	if a := doc.Find(".sm2-playlist-bd li a[href*='.mp3']").First(); a.Length() > 0 {
		d.AudioURL = abs(a.AttrOr("href", ""))
		d.FileSize = text(a.Find(".label"))
	}
	if d.FileSize == "" {
		dl := text(doc.Find("#downloadBtn"))
		if i := strings.LastIndex(dl, "|"); i >= 0 {
			d.FileSize = strings.TrimSpace(dl[i+1:])
		}
	}
	d.VideoURL = abs(doc.Find("a[href^='/watch/video/']").First().AttrOr("href", ""))

	if self, ok := parseSongPath(pagePath); ok {
		counts := map[string]int{}
		var best SongRef
		doc.Find("a[href*='/song/']").Each(func(_ int, a *goquery.Selection) {
			ref, ok := parseSongPath(a.AttrOr("href", ""))
			if !ok || ref.ID != self.ID || ref.Slug == "x" || ref.ArtistSlug == "x" {
				return
			}
			counts[ref.PagePath]++
			if counts[ref.PagePath] > counts[best.PagePath] {
				best = ref
			}
		})
		if best.ID != 0 {
			best.Title, best.Artist = d.Title, d.Artist
			d.Canonical = best
		}
	}

	page := text(doc.Find("body"))
	if m := ratingRe.FindStringSubmatch(page); m != nil {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			d.Rating = &f
		}
	}
	if m := votesRe.FindStringSubmatch(page); m != nil {
		d.Votes = intPtr(strings.ReplaceAll(m[1], ",", ""))
	}
	if d.Title == "" && d.AudioURL == "" {
		return d, ErrNoSong
	}
	return d, nil
}
