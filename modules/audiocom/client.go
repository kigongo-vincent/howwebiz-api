package audiocom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const PageSize = 10 // fixed by the API

// Client calls the audio.com public API, staying under its rate limit
// (60 requests/minute per user) and retrying 429s and 5xx with backoff.
type Client struct {
	APIBase string
	OAuth   *OAuth
	http    *http.Client
	gap     time.Duration // min spacing between requests

	mu   sync.Mutex
	next time.Time
}

func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	slot := c.next
	if now := time.Now(); slot.Before(now) {
		slot = now
	}
	c.next = slot.Add(c.gap)
	c.mu.Unlock()
	t := time.NewTimer(time.Until(slot))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pushBack delays every caller (server told us to slow down).
func (c *Client) pushBack(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := time.Now().Add(d); t.After(c.next) {
		c.next = t
	}
}

// Search calls GET /v1/search. query "" browses everything newest first;
// "#tag" matches a tag exactly. types: "audio", "author", "collection"
// (comma-separated, "" = all).
func (c *Client) Search(ctx context.Context, query, types string, page int) ([]SearchResult, error) {
	q := url.Values{"page": {strconv.Itoa(page)}}
	if query != "" {
		q.Set("query", query)
	}
	if types != "" {
		q.Set("types", types)
	}
	var out []SearchResult
	return out, c.get(ctx, "/v1/search?"+q.Encode(), &out)
}

func (c *Client) get(ctx context.Context, path string, v any) error {
	refreshed := false
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		token, err := c.OAuth.Token(ctx, false)
		if err != nil {
			return err
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBase+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			c.pushBack(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			err := json.NewDecoder(base.LimitBody(resp.Body)).Decode(v)
			resp.Body.Close()
			return err
		case resp.StatusCode == http.StatusUnauthorized && !refreshed:
			resp.Body.Close()
			refreshed = true
			if _, err := c.OAuth.Token(ctx, true); err != nil {
				return err
			}
			attempt-- // the refresh retry is free
			continue
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			wait := time.Duration(attempt+1) * 5 * time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
				wait = time.Duration(s)*time.Second + 500*time.Millisecond
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("GET %s: %d", path, resp.StatusCode)
			c.pushBack(wait)
			continue
		default:
			var e struct {
				Message string `json:"message"`
			}
			_ = json.NewDecoder(base.LimitBody(resp.Body)).Decode(&e)
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				return errors.Join(ErrNotConnected, fmt.Errorf("audio.com rejected the token: %s", e.Message))
			}
			return fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, e.Message)
		}
	}
	return lastErr
}
