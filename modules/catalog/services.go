package catalog

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"gorm.io/gorm"
)

// ProviderInfo describes a registered provider for /providers.
type ProviderInfo struct {
	Name        string `json:"name"`
	Site        string `json:"site"`
	Ingest      string `json:"ingest"` // "index" (own store, pulled by the indexer) | "push" (crawler writes here)
	LiveSearch  bool   `json:"liveSearch"`
	Description string `json:"description"`
}

// Service is the catalog's HTTP adapter.
type Service struct {
	DB        *gorm.DB
	Search    *SearchService
	Indexer   *Indexer
	Ingester  *Ingester
	Providers []ProviderInfo
}

var sorts = []string{"relevance", "recent", "plays", "title"}

// List is the aggregated songs endpoint.
//
//	?q=         text (title, artist, featured artists; accent/punctuation-insensitive)
//	?artist=    primary artist
//	?genre=  ?provider=  ?hasAudio=
//	?sort=      relevance (default with q) | recent | plays | title
//	?live=      auto (default: page 1 of a text search also asks every provider live) | true | false
//	?page= ?limit=
func (s *Service) List(c *fiber.Ctx) error {
	p := base.ParsePage(c)
	q := Query{Q: c.Query("q"), Artist: c.Query("artist"), Genre: c.Query("genre"), Provider: c.Query("provider"),
		Sort: c.Query("sort"), Page: p.Page, Limit: p.Limit, Live: LiveMode(c.Query("live", string(LiveAuto)))}
	if q.Sort != "" && !slices.Contains(sorts, q.Sort) {
		return base.API_ERROR(c, "sort must be one of: "+strings.Join(sorts, ", "))
	}
	if q.Live != LiveAuto && q.Live != LiveOn && q.Live != LiveOff {
		return base.API_ERROR(c, "live must be auto, true or false")
	}
	if q.Provider != "" && !slices.ContainsFunc(s.Providers, func(pi ProviderInfo) bool { return pi.Name == q.Provider }) {
		return base.API_ERROR(c, "unknown provider "+q.Provider+"; see /api/v1/providers")
	}
	if v := c.Query("hasAudio"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return base.API_ERROR(c, "hasAudio must be true or false")
		}
		q.HasAudio = &b
	}
	res, err := s.Search.Search(c.UserContext(), q)
	if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to search songs")
	}
	for i := range res.Songs {
		s.orderSources(&res.Songs[i])
	}
	meta := fiber.Map{"query": q.Q, "cached": res.Cached}
	if res.Live != nil {
		meta["live"] = res.Live
		meta["ingested"] = res.Ingested
	}
	return c.JSON(base.APIResponse{Data: res.Songs, Pagination: p.Controls(res.Total, len(res.Songs)), Meta: meta})
}

func (s *Service) orderSources(song *Song) {
	slices.SortStableFunc(song.Sources, func(a, b Source) int {
		return s.Ingester.rank(a.Provider) - s.Ingester.rank(b.Provider)
	})
}

func (s *Service) Get(c *fiber.Ctx) error {
	id, ok := base.ParamID(c, "id")
	if !ok {
		return base.API_ERROR(c, "invalid song id")
	}
	song, err := s.Search.Get(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return base.API_FAIL(c, fiber.StatusNotFound, "song not found")
	} else if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get song")
	}
	s.orderSources(&song)
	return c.JSON(base.APIResponse{Data: song})
}

type providerStatus struct {
	ProviderInfo
	Sources     int64      `json:"sources"`     // provider copies in the catalog
	IndexedUpTo *time.Time `json:"indexedUpTo"` // indexer watermark (index providers)
}

// ProvidersList shows every provider and how much of the catalog it backs.
func (s *Service) ProvidersList(c *fiber.Ctx) error {
	var counts []struct {
		Provider string
		N        int64
	}
	s.DB.Model(&Source{}).Select("provider, COUNT(*) AS n").Group("provider").Scan(&counts)
	byName := map[string]int64{}
	for _, r := range counts {
		byName[r.Provider] = r.N
	}
	var wms []Watermark
	s.DB.Find(&wms)
	wm := map[string]time.Time{}
	for _, w := range wms {
		wm[w.Provider] = w.Since
	}
	out := make([]providerStatus, 0, len(s.Providers))
	for _, p := range s.Providers {
		ps := providerStatus{ProviderInfo: p, Sources: byName[p.Name]}
		if t, ok := wm[p.Name]; ok {
			ps.IndexedUpTo = &t
		}
		out = append(out, ps)
	}
	var songs int64
	s.DB.Model(&Song{}).Count(&songs)
	var multi int64
	s.DB.Model(&Song{}).Where("source_count > 1").Count(&multi)
	return c.JSON(base.APIResponse{Data: out, Meta: fiber.Map{
		"songs": songs, "songsOnSeveralProviders": multi, "priority": s.Ingester.Priority,
		"lastIndexedAt": s.Indexer.LastRun()}})
}

// Reindex pulls everything changed from provider stores now.
func (s *Service) Reindex(c *fiber.Ctx) error {
	out := s.Indexer.RunOnce(c.UserContext())
	s.Search.Bump(c.UserContext())
	return c.JSON(base.APIResponse{Data: out, Message: "indexed"})
}

// Cleanup removes stored songs without an audio URL (data from before the
// audio-only rule; new data without audio is never stored).
func (s *Service) Cleanup(c *fiber.Ctx) error {
	st, err := s.Ingester.PurgeWithoutAudio(time.Now().UTC())
	if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "cleanup failed")
	}
	s.Search.Bump(c.UserContext())
	return c.JSON(base.APIResponse{Data: st, Message: "removed songs without audio"})
}
