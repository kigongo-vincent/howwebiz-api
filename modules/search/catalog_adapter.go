package search

import (
	"context"
	"net/url"
	"strconv"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"github.com/kigongo-vincent/howwebiz-api/modules/song"
	"gorm.io/gorm"
)

// CatalogSearcher runs howwe.ug's own search engine (Google Programmable
// Search) for the aggregated /songs search. Unavailable without
// GOOGLE_CSE_KEY.
type CatalogSearcher struct {
	Howwe *HowweSearch
	DB    *gorm.DB
}

func (CatalogSearcher) Name() string { return "howwe" }

func (s CatalogSearcher) Search(ctx context.Context, q string, limit int) ([]catalog.Track, error) {
	if !s.Howwe.Enabled() {
		return nil, catalog.ErrUnavailable
	}
	res, _, err := s.Howwe.Search(ctx, q, 1)
	if err != nil {
		return nil, err
	}
	var out []catalog.Track
	var ids []int64
	for _, it := range res.Items {
		u, err := url.Parse(it.Link)
		if err != nil {
			continue
		}
		ref, ok := scraper.ParseSongPath(u.Path)
		title, artist := splitSongTitle(it.Title)
		if !ok || title == "" {
			continue
		}
		out = append(out, catalog.Track{Provider: "howwe", ExternalID: strconv.FormatInt(ref.ID, 10),
			Title: title, Artist: artist, PageURL: it.Link})
		ids = append(ids, ref.ID)
		if len(out) == limit {
			break
		}
	}
	// Google results carry no mp3: take it from howwe songs already crawled
	// (the catalog keeps only songs with audio; the rest are crawled later).
	if len(ids) > 0 && s.DB != nil {
		var rows []song.Song
		s.DB.WithContext(ctx).Select("id", "audio_url", "cover_url", "genre").Where("id IN ? AND audio_url <> ''", ids).Find(&rows)
		byID := map[string]song.Song{}
		for _, r := range rows {
			byID[strconv.FormatUint(uint64(r.ID), 10)] = r
		}
		for i := range out {
			if r, ok := byID[out[i].ExternalID]; ok {
				out[i].AudioURL, out[i].CoverURL, out[i].Genre = r.AudioURL, r.CoverURL, r.Genre
			}
		}
	}
	return out, nil
}
