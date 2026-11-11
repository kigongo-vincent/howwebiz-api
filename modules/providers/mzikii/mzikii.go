// Package mzikii is the mzikii.com provider: Ugandan music, ~7.6k songs.
// robots.txt allows everything (Googlebot has its own rule).
//
//	/sitemap.xml               every song page (songs.php?song_id=N) with <lastmod>
//	/songs.php?song_id=N       song page with schema.org MusicRecording JSON-LD:
//	                           name, artist, image, audio contentUrl, canonical url
//
// The crawler records the mp3 URLs and never fetches them.
package mzikii

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

const Name = "mzikii"

type Site struct{ Base string } // https://www.mzikii.com

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: s.Base, Ingest: "push", LiveSearch: false,
		Description: "mzikii.com — Ugandan music (sitemap + song pages)"}
}

func (s Site) SitemapPaths() []string { return []string{"/sitemap.xml"} }

func (s Site) IsSongPage(path string) bool { return path == "/songs.php" }

var (
	ldRe = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)
	// canonical url: /song/{slug}/{id}/artist/{artist}
	idRe = regexp.MustCompile(`/song/[^/]+/(\d+)`)
)

type recording struct {
	Type     string `json:"@type"`
	Name     string `json:"name"`
	ByArtist struct {
		Name string `json:"name"`
	} `json:"byArtist"`
	Image string `json:"image"`
	URL   string `json:"url"`
	Audio struct {
		ContentURL string `json:"contentUrl"`
	} `json:"audio"`
	Genre string `json:"genre"`
}

// ParseSongPage reads the page's MusicRecording.
func (s Site) ParseSongPage(body []byte) ([]catalog.Track, error) {
	for _, m := range ldRe.FindAllSubmatch(body, -1) {
		var r recording
		if json.Unmarshal(m[1], &r) != nil || r.Type != "MusicRecording" {
			continue
		}
		id := idRe.FindStringSubmatch(r.URL)
		name := strings.TrimSpace(r.Name)
		if id == nil || name == "" || r.Audio.ContentURL == "" {
			return nil, sitecrawl.ErrNotFound
		}
		t := catalog.Track{Provider: Name, ExternalID: id[1], Title: name, Artist: strings.TrimSpace(r.ByArtist.Name),
			AudioURL: escapeURL(r.Audio.ContentURL), CoverURL: escapeURL(abs(r.Image)),
			PageURL: s.Base + "/songs.php?song_id=" + id[1]}
		if g := strings.TrimSpace(r.Genre); g != "" && !strings.EqualFold(g, "music") { // "Music" is the site's placeholder
			t.Genre = g
		}
		return []catalog.Track{t}, nil
	}
	return nil, sitecrawl.ErrNotFound
}

// abs: images come protocol-relative ("//mzikii.com/...").
func abs(u string) string {
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

// escapeURL escapes the spaces and brackets the site leaves raw in file names.
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
