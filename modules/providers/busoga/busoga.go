// Package busoga is the Busoga Music Store provider
// (busogamusicstore.com): music from Busoga (eastern Uganda), ~1.4k songs.
// robots.txt answers 410 Gone, which means no restrictions.
//
// Song pages answer only with their name slug (listen-music.php?songid=N
// alone is 410), so ids can't be probed, and there is no sitemap. Each feed
// is a listing page; FeedPage reads it, then each song page it links to
// (schema.org MusicRecording JSON-LD: name, artist, image, date, mp3 path).
// The JSON-LD duration is not the file's (1:04 for a 3.3 MB mp3) and is
// ignored. Plays and categories are behind a login and not used.
package busoga

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
)

const Name = "busoga"

// MaxSongsPerFeed bounds the song pages one listing costs per run (default).
const MaxSongsPerFeed = 150

type Site struct {
	Base     string // https://www.busogamusicstore.com
	MaxSongs int    // song pages per listing per run (0 = MaxSongsPerFeed)
}

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: s.Base, Ingest: "push", LiveSearch: false,
		Description: "busogamusicstore.com — music from Busoga, eastern Uganda (listing + song pages)"}
}

// Feeds are the listing pages (none paginates).
func (s Site) Feeds() []string { return []string{"/music.php", "/"} } // /top-rank.php is 404

func (s Site) Tail(string) bool { return false }

var (
	songLinkRe = regexp.MustCompile(`listen-music\.php\?songid=(\d+)&(?:amp;)?sn=([^"'&<>\s]+)`)
	ldRe       = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)
)

// SongPaths lists the song pages a listing links to, once each.
func SongPaths(listing []byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range songLinkRe.FindAllSubmatch(listing, -1) {
		if id := string(m[1]); !seen[id] {
			seen[id] = true
			out = append(out, "/listen-music.php?songid="+id+"&sn="+string(m[2]))
		}
	}
	return out
}

func (s Site) FeedPage(ctx context.Context, get sitecrawl.Fetch, feed, cursor string) ([]catalog.Track, string, bool, error) {
	listing, err := get(ctx, feed)
	if err != nil {
		return nil, "", false, err
	}
	var out []catalog.Track
	paths := SongPaths(listing)
	limit := s.MaxSongs
	if limit <= 0 {
		limit = MaxSongsPerFeed
	}
	paths = paths[:min(len(paths), limit)]
	for _, p := range paths {
		body, err := get(ctx, p)
		if errors.Is(err, webguard.ErrNotFound) {
			continue // removed since the listing was built
		}
		if err != nil {
			return out, "", false, err
		}
		if t, err := s.ParseSong(body); err == nil {
			out = append(out, t)
		}
	}
	return out, "", true, nil
}

type recording struct {
	Type     string `json:"@type"`
	Name     string `json:"name"`
	ByArtist struct {
		Name string `json:"name"`
	} `json:"byArtist"`
	Image      string `json:"image"`
	URL        string `json:"url"`
	Published  string `json:"datePublished"`
	ContentURL string `json:"contentUrl"`
}

var idRe = regexp.MustCompile(`songid=(\d+)`)

// ParseSong reads a song page's MusicRecording.
func (s Site) ParseSong(body []byte) (catalog.Track, error) {
	for _, m := range ldRe.FindAllSubmatch(body, -1) {
		var r recording
		if json.Unmarshal(m[1], &r) != nil || r.Type != "MusicRecording" {
			continue
		}
		id := idRe.FindStringSubmatch(r.URL)
		if id == nil || strings.TrimSpace(r.Name) == "" || r.ContentURL == "" {
			break
		}
		t := catalog.Track{Provider: Name, ExternalID: id[1], Title: strings.TrimSpace(r.Name),
			Artist: strings.TrimSpace(r.ByArtist.Name), AudioURL: s.abs(r.ContentURL), CoverURL: s.abs(r.Image), PageURL: r.URL}
		if d, err := time.Parse(time.RFC3339, r.Published); err == nil {
			t.ReleasedAt = &d
		}
		return t, nil
	}
	return catalog.Track{}, sitecrawl.ErrNotFound
}

// abs resolves the site's relative, space-containing file paths
// ("Africa Music/B2c/Namala by B2C.mp3") into escaped absolute URLs.
func (s Site) abs(p string) string {
	if p == "" {
		return ""
	}
	base, err := url.Parse(s.Base + "/")
	if err != nil {
		return p
	}
	ref, err := url.Parse(strings.ReplaceAll(p, " ", "%20"))
	if err != nil {
		return p
	}
	return base.ResolveReference(ref).String()
}
