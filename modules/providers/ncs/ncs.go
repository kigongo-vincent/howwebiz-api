// Package ncs is the NoCopyrightSounds provider (ncs.io): free-to-use
// electronic, pop, hip hop and more, released for creators. robots.txt
// allows everything.
//
//	/?display=&page=N   newest releases, each card a .player-play element whose
//	                    data-* attributes carry id, title, artist, genre, cover
//	                    and the full mp3 URL
//
// Document-based: plain HTML, no API.
package ncs

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
)

const Name = "ncs"

type Site struct{ Base string } // https://ncs.io

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: s.Base, Ingest: "push", LiveSearch: false,
		Description: "ncs.io — NoCopyrightSounds: free-to-use electronic, pop, hip hop releases (listing pages)"}
}

func (s Site) ListPath(page int) string { return "/?display=&page=" + strconv.Itoa(page) }

func (s Site) ParseList(body []byte) ([]catalog.Track, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out []catalog.Track
	seen := map[string]bool{}
	doc.Find(".player-play[data-tid][data-url]").Each(func(_ int, el *goquery.Selection) {
		attr := func(k string) string { return strings.TrimSpace(el.AttrOr("data-"+k, "")) }
		id, title, audio := attr("tid"), attr("track"), attr("url")
		if id == "" || seen[id] || title == "" || !strings.HasSuffix(strings.ToLower(audio), ".mp3") {
			return
		}
		seen[id] = true
		out = append(out, catalog.Track{Provider: Name, ExternalID: id, Title: title, Artist: attr("artistraw"),
			Genre: attr("genre"), CoverURL: attr("cover"), AudioURL: audio, PageURL: s.Base + "/track/" + id})
	})
	return out, nil
}
