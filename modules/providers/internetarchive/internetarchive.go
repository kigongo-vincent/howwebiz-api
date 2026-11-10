// Package internetarchive is the Internet Archive provider
// (https://archive.org): full-length recordings that their rights holders
// let the Archive distribute, from netlabels and artists worldwide.
//
// Default collections (INTERNETARCHIVE_COLLECTIONS):
//
//	netlabels  ~80k releases from netlabels, mostly Creative Commons
//	etree      Live Music Archive: ~300k concerts by trade-friendly artists
//
// robots.txt allows /advancedsearch.php, /metadata and /download; the
// Archive asks bots for a descriptive User-Agent and to honour 429 +
// Retry-After (webguard does both).
//
//	/advancedsearch.php?q=collection:(C) AND addeddate:[W TO null]&sort[]=addeddate asc   items, oldest added first
//	/metadata/{identifier}                                                            an item's files
//	/download/{identifier}/{file}                                                     the full mp3
//
// Each collection is a tail feed: the cursor is "watermark|page", and the
// watermark moves to the last addeddate seen before the 10k paging cap.
package internetarchive

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
)

const (
	Name     = "internetarchive"
	maxStart = 10000 // advancedsearch refuses deeper paging
)

var DefaultCollections = []string{"netlabels", "etree"}

type Site struct {
	Base        string // https://archive.org
	Collections []string
	Items       int // items per feed page (each costs one /metadata request)
}

func (s Site) Name() string { return Name }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: s.Base, Ingest: "push", LiveSearch: true,
		Description: "archive.org — netlabel releases and the Live Music Archive (" + strings.Join(s.Collections, ", ") + "): full-length mp3s, per-item licences"}
}

func (s Site) Feeds() []string  { return s.Collections }
func (s Site) Tail(string) bool { return true }

func (s Site) rows() int { return min(max(s.Items, 1), 100) }

func splitCursor(c string) (string, int) {
	w, p, _ := strings.Cut(c, "|")
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 {
		n = 1
	}
	return w, n
}

type doc struct {
	Identifier string `json:"identifier"`
	AddedDate  string `json:"addeddate"`
}

func (s Site) search(ctx context.Context, get sitecrawl.Fetch, q string, rows, page int, sorts ...string) ([]doc, error) {
	v := url.Values{"q": {q}, "fl[]": {"identifier", "addeddate"}, "rows": {strconv.Itoa(rows)},
		"page": {strconv.Itoa(page)}, "output": {"json"}}
	for _, so := range sorts {
		v.Add("sort[]", so)
	}
	body, err := get(ctx, "/advancedsearch.php?"+v.Encode())
	if err != nil {
		return nil, err
	}
	var res struct {
		Response struct {
			Docs []doc `json:"docs"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("internetarchive search: %w", err)
	}
	return res.Response.Docs, nil
}

func (s Site) FeedPage(ctx context.Context, get sitecrawl.Fetch, feed, cursor string) ([]catalog.Track, string, bool, error) {
	mark, page := splitCursor(cursor)
	q := "collection:(" + feed + ")"
	if mark != "" {
		q += " AND addeddate:[" + mark + " TO null]"
	}
	rows := s.rows()
	docs, err := s.search(ctx, get, q, rows, page, "addeddate asc", "identifier asc")
	if err != nil {
		return nil, "", false, err
	}
	var tracks []catalog.Track
	for _, d := range docs {
		more, err := s.item(ctx, get, d.Identifier, feed == "etree")
		if err != nil {
			if fatalErr(err) || ctx.Err() != nil {
				return nil, "", false, err
			}
			continue // one broken item does not stop the feed
		}
		tracks = append(tracks, more...)
	}
	switch {
	case len(docs) < rows: // the end: resume from here (new items are added after it)
		return tracks, cursor, true, nil
	case (page+1)*rows > maxStart && len(docs) > 0: // re-anchor before the paging cap
		return tracks, docs[len(docs)-1].AddedDate + "|1", false, nil
	default:
		return tracks, mark + "|" + strconv.Itoa(page+1), false, nil
	}
}

func fatalErr(err error) bool {
	return errors.Is(err, webguard.ErrBlocked) || errors.Is(err, webguard.ErrCircuitOpen)
}

var unsafeQuery = regexp.MustCompile(`[^\p{L}\p{N}\s'-]+`)

func (s Site) SearchAPI(ctx context.Context, get sitecrawl.Fetch, q string, limit int) ([]catalog.Track, error) {
	q = strings.TrimSpace(unsafeQuery.ReplaceAllString(q, " "))
	if q == "" {
		return nil, nil
	}
	query := fmt.Sprintf("collection:(%s) AND (title:(%s) OR creator:(%s))", strings.Join(s.Collections, " OR "), q, q)
	docs, err := s.search(ctx, get, query, 3, 1)
	if err != nil {
		return nil, err
	}
	var out []catalog.Track
	for _, d := range docs {
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < 2*time.Second {
			break // return what we have rather than time out
		}
		tracks, err := s.item(ctx, get, d.Identifier, false)
		if err != nil {
			continue
		}
		out = append(out, tracks...)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

type file struct {
	Name     string `json:"name"`
	Source   string `json:"source"`
	Format   string `json:"format"`
	Length   string `json:"length"`
	Title    string `json:"title"`
	Track    string `json:"track"`
	Creator  string `json:"creator"`
	Artist   string `json:"artist"`
	Original string `json:"original"`
}

type metadata struct {
	Metadata map[string]any `json:"metadata"`
	Files    []file         `json:"files"`
}

func (s Site) item(ctx context.Context, get sitecrawl.Fetch, id string, live bool) ([]catalog.Track, error) {
	body, err := get(ctx, "/metadata/"+url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	return s.ParseItem(id, body, live)
}

// ParseItem returns one track per audio file of an item (its mp3, which
// the Archive derives from the original). live: concert recordings get a
// "(Live)" suffix so they never merge with studio versions.
func (s Site) ParseItem(id string, body []byte, live bool) ([]catalog.Track, error) {
	var m metadata
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("internetarchive metadata %s: %w", id, err)
	}
	if len(m.Metadata) == 0 {
		return nil, sitecrawl.ErrNotFound // dark or removed item
	}
	byName := map[string]file{}
	for _, f := range m.Files {
		byName[f.Name] = f
	}
	itemCreator := str(m.Metadata["creator"])
	released := parseDate(str(m.Metadata["date"]))
	var genre string
	if subj := strs(m.Metadata["subject"]); len(subj) > 0 {
		genre = subj[0]
	}
	// Prefer the VBR derivative; fall back to any mp3 (e.g. an uploaded original).
	pick := map[string]file{} // key: the original it stands for
	for _, f := range m.Files {
		if !strings.Contains(f.Format, "MP3") || !strings.HasSuffix(strings.ToLower(f.Name), ".mp3") {
			continue
		}
		key := f.Name
		if f.Original != "" {
			key = f.Original
		}
		if cur, ok := pick[key]; !ok || (f.Format == "VBR MP3" && cur.Format != "VBR MP3") {
			pick[key] = f
		}
	}
	var out []catalog.Track
	for _, f := range m.Files { // keep the item's file order
		if pf, ok := pick[orKey(f)]; !ok || pf.Name != f.Name {
			continue
		}
		orig := byName[f.Original]
		title := first(f.Title, orig.Title)
		if title == "" {
			title = fileTitle(f.Name)
		}
		if live {
			title += " (Live)"
		}
		artist := first(f.Creator, f.Artist, orig.Creator, orig.Artist, itemCreator)
		if title == "" || artist == "" {
			continue
		}
		ext := id + "/" + f.Name
		if len(ext) > 128 {
			h := sha1.Sum([]byte(f.Name))
			ext = id[:min(len(id), 100)] + "/" + hex.EncodeToString(h[:12])
		}
		out = append(out, catalog.Track{Provider: Name, ExternalID: ext, Title: title, Artist: artist, Genre: genre,
			AudioURL:   s.Base + "/download/" + url.PathEscape(id) + "/" + escapePath(f.Name),
			CoverURL:   s.Base + "/services/img/" + url.PathEscape(id),
			PageURL:    s.Base + "/details/" + url.PathEscape(id),
			DurationMs: lengthMs(first(f.Length, orig.Length)), ReleasedAt: released})
	}
	return out, nil
}

func orKey(f file) string {
	if f.Original != "" {
		return f.Original
	}
	return f.Name
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, x := range parts {
		parts[i] = url.PathEscape(x)
	}
	return strings.Join(parts, "/")
}

var leadingTrackNo = regexp.MustCompile(`^\s*\d{1,3}\s*[-_.)\s]+\s*`)

// fileTitle makes a title from a file name: "04_Artist - Song Name.mp3" → "Artist - Song Name".
func fileTitle(name string) string {
	t := strings.TrimSuffix(path.Base(name), path.Ext(name))
	t = leadingTrackNo.ReplaceAllString(t, "")
	return strings.TrimSpace(strings.ReplaceAll(t, "_", " "))
}

// lengthMs parses "184.32" (seconds) or "mm:ss" / "hh:mm:ss".
func lengthMs(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if !strings.Contains(s, ":") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return int(f * 1000)
	}
	total := 0.0
	for _, p := range strings.Split(s, ":") {
		n, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return int(total * 1000)
}

func parseDate(s string) *time.Time {
	for _, layout := range []string{time.RFC3339, time.DateOnly, "2006-01", "2006"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

func first(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// str reads a metadata value that may be a string or a list of strings.
func str(v any) string {
	if l := strs(v); len(l) > 0 {
		return l[0]
	}
	return ""
}

func strs(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
