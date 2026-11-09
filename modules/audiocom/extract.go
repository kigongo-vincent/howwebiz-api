package audiocom

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Page extraction for audio.com's website (a Next.js App Router app).
//
// audio.com's real markup could not be inspected while building this
// (every page answered with a Cloudflare challenge), so the extractor does
// not depend on CSS classes. It reads the structured data Next.js and SEO
// tags embed in the HTML and picks out objects shaped like audio:
//
//  1. React Server Components flight data: self.__next_f.push([1,"..."])
//  2. Pages-router data: <script id="__NEXT_DATA__">
//  3. JSON-LD: MusicRecording / AudioObject
//
// Field names are accepted in both the API's snake_case and camelCase.

var (
	rscPushRe   = regexp.MustCompile(`self\.__next_f\.push\(\[1,\s*("(?:[^"\\]|\\.)*")\]\)`)
	nextDataRe  = regexp.MustCompile(`(?s)<script[^>]+id="__NEXT_DATA__"[^>]*>(.*?)</script>`)
	jsonLDRe    = regexp.MustCompile(`(?s)<script[^>]+type="application/ld\+json"[^>]*>(.*?)</script>`)
	genreHrefRe = regexp.MustCompile(`^(?:https?://(?:www\.)?audio\.com)?/genres/([A-Za-z0-9][A-Za-z0-9_%-]*)/?$`)
	isoDurRe    = regexp.MustCompile(`^PT(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?$`)
)

// PageData is what one page yielded.
type PageData struct {
	Tracks []apiAudio
	Genres []string // genre slugs linked from the page
	Title  string
	// Sources lists which embedded formats were found (for diagnostics).
	Sources []string
}

func ExtractPage(body []byte) PageData {
	var pd PageData
	seen := map[string]bool{}
	add := func(a apiAudio) {
		if a.ID != "" && !seen[a.ID] {
			seen[a.ID] = true
			pd.Tracks = append(pd.Tracks, a)
		}
	}

	// 1. RSC flight data.
	if ms := rscPushRe.FindAllSubmatch(body, -1); len(ms) > 0 {
		pd.Sources = append(pd.Sources, "rsc")
		var flight strings.Builder
		for _, m := range ms {
			var chunk string
			if json.Unmarshal(m[1], &chunk) == nil {
				flight.WriteString(chunk)
			}
		}
		for line := range strings.SplitSeq(flight.String(), "\n") {
			_, payload, ok := strings.Cut(line, ":")
			if !ok || payload == "" || (payload[0] != '[' && payload[0] != '{') {
				continue
			}
			var v any
			if json.Unmarshal([]byte(payload), &v) == nil {
				walk(v, add)
			}
		}
	}
	// 2. __NEXT_DATA__.
	if m := nextDataRe.FindSubmatch(body); m != nil {
		pd.Sources = append(pd.Sources, "next_data")
		var v any
		if json.Unmarshal(m[1], &v) == nil {
			walk(v, add)
		}
	}
	// 3. JSON-LD.
	for _, m := range jsonLDRe.FindAllSubmatch(body, -1) {
		var v any
		if json.Unmarshal(bytes.TrimSpace(m[1]), &v) == nil {
			pd.Sources = append(pd.Sources, "json_ld")
			walkLD(v, add)
		}
	}

	if doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body)); err == nil {
		pd.Title = strings.TrimSpace(doc.Find("title").First().Text())
		gs := map[string]bool{}
		doc.Find("a[href]").Each(func(_ int, a *goquery.Selection) {
			if m := genreHrefRe.FindStringSubmatch(a.AttrOr("href", "")); m != nil && !gs[m[1]] {
				gs[m[1]] = true
				pd.Genres = append(pd.Genres, m[1])
			}
		})
	}
	return pd
}

// walk visits every map in v; audio-shaped ones are passed to add.
func walk(v any, add func(apiAudio)) {
	switch x := v.(type) {
	case map[string]any:
		if a, ok := audioFromMap(x); ok {
			add(a)
		}
		for _, c := range x {
			walk(c, add)
		}
	case []any:
		for _, c := range x {
			walk(c, add)
		}
	}
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return ""
}

func num(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t.Unix()
			}
		}
	}
	return 0
}

// audioFromMap recognizes an audio object: an id and a title plus at
// least one audio-specific field. Collections and authors are skipped.
func audioFromMap(m map[string]any) (apiAudio, bool) {
	id, title := str(m, "id", "audioId", "audio_id"), str(m, "title")
	if id == "" || title == "" {
		return apiAudio{}, false
	}
	if t := str(m, "type", "__typename"); t == "collection" || t == "author" || strings.EqualFold(t, "Collection") {
		return apiAudio{}, false
	}
	if _, ok := m["items_count"]; ok {
		return apiAudio{}, false
	}
	if _, ok := m["itemsCount"]; ok {
		return apiAudio{}, false
	}
	play, _ := m["play"].(map[string]any)
	playURL := str(m, "playUrl", "play_url", "audioUrl", "streamUrl")
	if play != nil {
		if u := str(play, "url"); u != "" {
			playURL = u
		}
	}
	username := str(m, "username", "userName", "ownerUsername")
	if author, ok := m["author"].(map[string]any); ok && username == "" {
		username = str(author, "username", "userName")
	}
	audioish := playURL != "" || (username != "" && str(m, "slug") != "") || m["waveform"] != nil || m["duration"] != nil
	if !audioish {
		return apiAudio{}, false
	}

	a := apiAudio{
		ID: id, Title: title, Slug: str(m, "slug"), Username: username,
		AuthorName:  str(m, "author_name", "authorName"),
		Description: str(m, "description"), Category: str(m, "category"), License: str(m, "license"),
		Image:       str(m, "image", "imageUrl", "cover", "coverUrl"),
		IsDownload:  m["is_downloadable"] == true || m["isDownloadable"] == true,
		DateCreated: num(m, "date_created", "dateCreated", "createdAt", "created_at"),
		DateUpdated: num(m, "date_updated", "dateUpdated", "updatedAt", "updated_at"),
	}
	if author, ok := m["author"].(map[string]any); ok && a.AuthorName == "" {
		a.AuthorName = str(author, "name", "displayName")
	}
	if tags, ok := m["tags"].([]any); ok {
		for _, t := range tags {
			switch tv := t.(type) {
			case string:
				a.Tags = append(a.Tags, tv)
			case map[string]any:
				if n := str(tv, "name", "slug", "title"); n != "" {
					a.Tags = append(a.Tags, n)
				}
			}
		}
	}
	a.Play.URL = playURL
	if play != nil {
		a.Play.Duration = int(num(play, "duration"))
		a.Play.Waveform = str(play, "waveform")
	}
	if a.Play.Duration == 0 {
		a.Play.Duration = int(num(m, "duration", "durationMs"))
	}
	return a, true
}

// walkLD extracts schema.org MusicRecording / AudioObject items.
func walkLD(v any, add func(apiAudio)) {
	switch x := v.(type) {
	case []any:
		for _, c := range x {
			walkLD(c, add)
		}
	case map[string]any:
		if g, ok := x["@graph"]; ok {
			walkLD(g, add)
		}
		if items, ok := x["itemListElement"]; ok {
			walkLD(items, add)
		}
		if item, ok := x["item"]; ok {
			walkLD(item, add)
		}
		t := str(x, "@type")
		if t != "MusicRecording" && t != "AudioObject" {
			return
		}
		pageURL := str(x, "url", "@id")
		a := apiAudio{Title: str(x, "name"), Description: str(x, "description"), Image: str(x, "image", "thumbnailUrl")}
		if u, err := url.Parse(pageURL); err == nil && u.Path != "" {
			a.ID = "url:" + strings.Trim(u.Path, "/")
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			a.Username, a.Slug = parts[0], parts[len(parts)-1]
		}
		for _, k := range []string{"byArtist", "author", "creator"} {
			if p, ok := x[k].(map[string]any); ok && a.AuthorName == "" {
				a.AuthorName = str(p, "name")
			}
		}
		a.Play.URL = str(x, "contentUrl")
		if au, ok := x["audio"].(map[string]any); ok && a.Play.URL == "" {
			a.Play.URL = str(au, "contentUrl")
		}
		if m := isoDurRe.FindStringSubmatch(str(x, "duration")); m != nil {
			h, _ := strconv.Atoi(m[1])
			mi, _ := strconv.Atoi(m[2])
			s, _ := strconv.ParseFloat(m[3], 64)
			a.Play.Duration = int((float64(h*3600+mi*60) + s) * 1000)
		}
		if kw := str(x, "keywords", "genre"); kw != "" {
			for k := range strings.SplitSeq(kw, ",") {
				if k = strings.TrimSpace(k); k != "" {
					a.Tags = append(a.Tags, k)
				}
			}
		}
		if a.ID != "" && a.Title != "" {
			add(a)
		}
	}
}

// describe summarizes a page for error messages.
func (pd PageData) describe(size int) string {
	return fmt.Sprintf("title=%q size=%dB embedded=%v", pd.Title, size, pd.Sources)
}
