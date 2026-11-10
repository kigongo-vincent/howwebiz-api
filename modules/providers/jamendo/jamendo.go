// Package jamendo is the Jamendo provider (https://www.jamendo.com):
// ~600k Creative Commons tracks by independent artists from around the
// world, streamed in full through the official API v3.0.
//
// Needs a free client_id from https://devportal.jamendo.com (JAMENDO_CLIENT_ID);
// without one the provider is listed but disabled. API terms §3.3: free
// for non-commercial use, commercial use needs a deal with Jamendo
// (licensing@jamendo.com); §4.1: credit the artist and Jamendo and link
// to the track page (PageURL). Each track's CC licence is per track.
//
//	/v3.0/tracks/?order=id_asc&offset=N&limit=200   every track, oldest id first (tail feed)
//	/v3.0/tracks/?search=q                          live search
package jamendo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

const (
	Name     = "jamendo"
	pageSize = 200 // the API's maximum
)

type Site struct {
	Base     string // https://api.jamendo.com
	ClientID string
}

func (s Site) Name() string  { return Name }
func (s Site) Enabled() bool { return s.ClientID != "" }

func (s Site) Info() catalog.ProviderInfo {
	return catalog.ProviderInfo{Name: Name, Site: "https://www.jamendo.com", Ingest: "push", LiveSearch: true,
		Description: "jamendo.com — Creative Commons music from independent artists worldwide; full streams via API v3.0 (needs JAMENDO_CLIENT_ID; non-commercial use)"}
}

// One feed: every track by ascending id. New tracks get higher ids, so
// the feed is a tail: each run resumes at the stored offset.
func (s Site) Feeds() []string  { return []string{"all"} }
func (s Site) Tail(string) bool { return true }
func (s Site) params() url.Values {
	return url.Values{"client_id": {s.ClientID}, "format": {"json"}, "include": {"musicinfo licenses"},
		"audioformat": {"mp32"}, "imagesize": {"600"}, "type": {"single albumtrack"}}
}

func (s Site) FeedPage(ctx context.Context, get sitecrawl.Fetch, _ string, cursor string) ([]catalog.Track, string, bool, error) {
	offset, _ := strconv.Atoi(cursor)
	q := s.params()
	q.Set("order", "id_asc")
	q.Set("limit", strconv.Itoa(pageSize))
	q.Set("offset", strconv.Itoa(offset))
	body, err := get(ctx, "/v3.0/tracks/?"+q.Encode())
	if err != nil {
		return nil, "", false, err
	}
	n, tracks, err := Parse(body)
	if err != nil {
		return nil, "", false, err
	}
	if n < pageSize { // the end: resume here next time (new ids land after it)
		return tracks, strconv.Itoa(offset + n), true, nil
	}
	return tracks, strconv.Itoa(offset + n), false, nil
}

func (s Site) SearchAPI(ctx context.Context, get sitecrawl.Fetch, q string, limit int) ([]catalog.Track, error) {
	v := s.params()
	v.Set("search", q)
	v.Set("limit", strconv.Itoa(min(max(limit, 10), pageSize)))
	body, err := get(ctx, "/v3.0/tracks/?"+v.Encode())
	if err != nil {
		return nil, err
	}
	_, tracks, err := Parse(body)
	return tracks, err
}

type response struct {
	Headers struct {
		Status       string `json:"status"`
		Code         int    `json:"code"`
		ErrorMessage string `json:"error_message"`
	} `json:"headers"`
	Results []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Duration    int    `json:"duration"`
		ArtistName  string `json:"artist_name"`
		AlbumImage  string `json:"album_image"`
		Image       string `json:"image"`
		Audio       string `json:"audio"`
		ReleaseDate string `json:"releasedate"`
		ShareURL    string `json:"shareurl"`
		MusicInfo   *struct {
			Tags struct {
				Genres []string `json:"genres"`
			} `json:"tags"`
		} `json:"musicinfo"`
	} `json:"results"`
}

// Parse returns how many results the page had and the playable tracks.
// An API error (bad client_id, suspended app) is an error, not an empty page.
func Parse(body []byte) (int, []catalog.Track, error) {
	var res response
	if err := json.Unmarshal(body, &res); err != nil {
		return 0, nil, fmt.Errorf("jamendo: %w", err)
	}
	if res.Headers.Status != "success" {
		return 0, nil, fmt.Errorf("jamendo api error %d: %s", res.Headers.Code, res.Headers.ErrorMessage)
	}
	out := make([]catalog.Track, 0, len(res.Results))
	for _, r := range res.Results {
		if r.ID == "" || r.Name == "" || r.Audio == "" {
			continue
		}
		t := catalog.Track{Provider: Name, ExternalID: r.ID, Title: r.Name, Artist: r.ArtistName, AudioURL: r.Audio,
			CoverURL: r.Image, PageURL: r.ShareURL, DurationMs: r.Duration * 1000}
		if t.CoverURL == "" {
			t.CoverURL = r.AlbumImage
		}
		if t.PageURL == "" {
			t.PageURL = "https://www.jamendo.com/track/" + r.ID
		}
		if r.MusicInfo != nil && len(r.MusicInfo.Tags.Genres) > 0 {
			t.Genre = r.MusicInfo.Tags.Genres[0]
		}
		if d, err := time.Parse(time.DateOnly, r.ReleaseDate); err == nil {
			t.ReleasedAt = &d
		}
		out = append(out, t)
	}
	return len(res.Results), out, nil
}
