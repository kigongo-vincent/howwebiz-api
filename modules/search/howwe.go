package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/base"
)

// HowweSearch queries the search engine howwe.ug's own /search page uses:
// a Google Programmable Search Engine (cx below), via Google's Custom
// Search JSON API. howwe.ug has no server-side search of its own.
//
// Caveats (Google, 2026): the JSON API is closed to new customers and is
// discontinued on 2027-01-01, and the free tier is 100 queries/day — hence
// the response cache.
type HowweSearch struct {
	Key      string
	CX       string
	Endpoint string
	TTL      time.Duration
	http     *http.Client

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	res     Result
	expires time.Time
}

const (
	howweCX      = "76ecd7f17cc7e4f1b" // from https://www.howwe.ug/search
	pageSize     = 10                  // Google's max per request
	googleMaxRes = 100                 // Google never returns past result 100
)

var ErrNotConfigured = errors.New("howwe search needs GOOGLE_CSE_KEY (Google Custom Search JSON API key)")

func HowweFromEnv() *HowweSearch {
	return &HowweSearch{
		Key:      base.Env("GOOGLE_CSE_KEY", ""),
		CX:       base.Env("GOOGLE_CSE_CX", howweCX),
		Endpoint: base.Env("GOOGLE_CSE_ENDPOINT", "https://www.googleapis.com/customsearch/v1"),
		TTL:      base.EnvDuration("SEARCH_CACHE_TTL", 6*time.Hour),
		http:     &http.Client{Timeout: 10 * time.Second},
		cache:    map[string]cached{},
	}
}

func (h *HowweSearch) Enabled() bool { return h != nil && h.Key != "" }

type Item struct {
	Title   string `json:"title"`
	Link    string `json:"link"`
	Snippet string `json:"snippet"`
}

type Result struct {
	Total int64
	Items []Item
}

// Search returns one page (10 results) of howwe.ug's search for q.
func (h *HowweSearch) Search(ctx context.Context, q string, page int) (res Result, fromCache bool, err error) {
	if !h.Enabled() {
		return res, false, ErrNotConfigured
	}
	start := (page-1)*pageSize + 1
	if start > googleMaxRes-pageSize+1 {
		return res, false, fmt.Errorf("howwe search returns at most %d results (page <= %d)", googleMaxRes, googleMaxRes/pageSize)
	}
	key := fmt.Sprintf("%d|%s", page, strings.ToLower(strings.TrimSpace(q)))
	h.mu.Lock()
	if c, ok := h.cache[key]; ok && time.Now().Before(c.expires) {
		h.mu.Unlock()
		return c.res, true, nil
	}
	h.mu.Unlock()

	v := url.Values{"key": {h.Key}, "cx": {h.CX}, "q": {q}, "num": {fmt.Sprint(pageSize)}, "start": {fmt.Sprint(start)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.Endpoint+"?"+v.Encode(), nil)
	if err != nil {
		return res, false, err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return res, false, fmt.Errorf("google search: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		SearchInformation struct {
			TotalResults string `json:"totalResults"`
		} `json:"searchInformation"`
		Items []Item `json:"items"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return res, false, fmt.Errorf("google search: decode: %w", err)
	}
	if body.Error != nil || resp.StatusCode != http.StatusOK {
		msg := resp.Status
		if body.Error != nil {
			msg = body.Error.Message
		}
		return res, false, fmt.Errorf("google search: %s", msg)
	}
	fmt.Sscan(body.SearchInformation.TotalResults, &res.Total)
	res.Items = body.Items
	if res.Items == nil {
		res.Items = []Item{}
	}

	h.mu.Lock()
	if len(h.cache) > 5000 { // crude bound; entries are tiny
		h.cache = map[string]cached{}
	}
	h.cache[key] = cached{res: res, expires: time.Now().Add(h.TTL)}
	h.mu.Unlock()
	return res, false, nil
}
