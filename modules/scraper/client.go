// Package scraper fetches and parses howwe.ug pages.
package scraper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Client is a polite HTTP client: it spaces requests by a minimum delay
// (shared across goroutines) and retries transient failures.
type Client struct {
	BaseURL   string
	userAgent string
	http      *http.Client
	delay     time.Duration

	mu   sync.Mutex
	next time.Time
}

func NewClient(baseURL, userAgent string, delay, timeout time.Duration) *Client {
	return &Client{
		BaseURL:   baseURL,
		userAgent: userAgent,
		http:      &http.Client{Timeout: timeout},
		delay:     delay,
	}
}

// wait blocks until this caller's slot in the shared request schedule.
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	now := time.Now()
	slot := c.next
	if slot.Before(now) {
		slot = now
	}
	c.next = slot.Add(c.delay)
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

var errNotFound = errors.New("not found")

// Get fetches path (relative to BaseURL) and returns the body.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	url := c.BaseURL + path
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * 2 * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		if err := c.wait(ctx); err != nil {
			return nil, err
		}
		body, retry, err := c.do(ctx, url)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return nil, lastErr
}

func (c *Client) do(ctx context.Context, url string) (body []byte, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, fmt.Errorf("GET %s: %w", url, errNotFound)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, true, fmt.Errorf("GET %s: read body: %w", url, err)
	}
	return body, false, nil
}
