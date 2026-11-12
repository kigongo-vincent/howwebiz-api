// Package httpcache keeps the read API answering while the database is
// slow, using Redis:
//
//   - every successful JSON GET on a cacheable path is stored twice: a fresh
//     copy (served as is for FreshTTL) and a stale copy (kept StaleTTL);
//   - when the handler fails (a 5xx: typically a statement timeout on a
//     strained database), the stale copy answers instead (X-Cache: STALE);
//   - the most requested URLs of the last hours are counted, and a Warmer
//     rebuilds them in the background before their fresh copy expires, so
//     popular pages are always ready and never wait on the database.
//
// Without Redis every request just goes to its handler.
package httpcache

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/cache"
)

type Options struct {
	Cache *cache.Cache
	// Prefixes: only paths starting with one of these are cached.
	Prefixes []string
	// Skip: paths containing one of these are never cached (status pages).
	Skip     []string
	FreshTTL time.Duration // default 2m
	StaleTTL time.Duration // default 24h
	MaxBody  int           // bytes; larger responses are not stored (default 1 MiB)
	// Shuffled reports requests whose answer must differ every time (a
	// random order): never served from the fresh copy (nor warmed), but
	// still kept as the stale fallback.
	Shuffled func(path string, query url.Values) bool
}

const (
	warmHeader = "X-Cache-Warm"
	hotTTL     = 3 * time.Hour
)

// warmToken marks the Warmer's own requests (a random value per process,
// so clients can't use the header to skip the cache).
var warmToken = func() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

func hotKey(t time.Time) string { return "http:hot:" + t.UTC().Format("2006010215") }

func (o *Options) defaults() {
	if o.FreshTTL <= 0 {
		o.FreshTTL = 2 * time.Minute
	}
	if o.StaleTTL <= 0 {
		o.StaleTTL = 24 * time.Hour
	}
	if o.MaxBody <= 0 {
		o.MaxBody = 1 << 20
	}
}

func (o Options) cacheable(path string) bool {
	for _, s := range o.Skip {
		if strings.Contains(path, s) {
			return false
		}
	}
	for _, p := range o.Prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// key is the path with its query sorted: two spellings of one request
// share an entry.
func key(path string, query []byte) string {
	q, _ := url.ParseQuery(string(query))
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode() // Encode sorts by key
}

// entry: [2-byte status][content type]\n[body]
func encode(status int, ct string, body []byte) []byte {
	var b bytes.Buffer
	var st [2]byte
	binary.BigEndian.PutUint16(st[:], uint16(status))
	b.Write(st[:])
	b.WriteString(ct)
	b.WriteByte('\n')
	b.Write(body)
	return b.Bytes()
}

func decode(b []byte) (status int, ct string, body []byte, ok bool) {
	if len(b) < 3 {
		return 0, "", nil, false
	}
	status = int(binary.BigEndian.Uint16(b[:2]))
	rest := b[2:]
	i := bytes.IndexByte(rest, '\n')
	if i < 0 {
		return 0, "", nil, false
	}
	return status, string(rest[:i]), rest[i+1:], true
}

func serve(c *fiber.Ctx, b []byte, how string) bool {
	status, ct, body, ok := decode(b)
	if !ok {
		return false
	}
	c.Status(status)
	c.Set(fiber.HeaderContentType, ct)
	c.Set("X-Cache", how)
	_ = c.Send(body)
	return true
}

// New returns the middleware.
func New(o Options) fiber.Handler {
	o.defaults()
	return func(c *fiber.Ctx) error {
		if o.Cache == nil || c.Method() != fiber.MethodGet || !o.cacheable(c.Path()) {
			return c.Next()
		}
		ctx := c.UserContext()
		k := key(c.Path(), c.Request().URI().QueryString())
		warming := c.Get(warmHeader) == warmToken
		shuffled := false
		if o.Shuffled != nil {
			qv, _ := url.ParseQuery(string(c.Request().URI().QueryString()))
			shuffled = o.Shuffled(c.Path(), qv)
		}
		if !warming && !shuffled {
			o.Cache.ZIncr(ctx, hotKey(time.Now()), k, hotTTL)
			if b, ok := o.Cache.GetBytes(ctx, "http:fresh:"+k); ok && serve(c, b, "HIT") {
				return nil
			}
		}
		err := c.Next()
		status := c.Response().StatusCode()
		if err == nil && status == fiber.StatusOK {
			ct := string(c.Response().Header.ContentType())
			body := c.Response().Body()
			if strings.HasPrefix(ct, fiber.MIMEApplicationJSON) && len(body) <= o.MaxBody {
				e := encode(status, ct, body)
				if !shuffled {
					o.Cache.SetBytes(ctx, "http:fresh:"+k, e, o.FreshTTL)
				}
				o.Cache.SetBytes(ctx, "http:stale:"+k, e, o.StaleTTL)
			}
			c.Set("X-Cache", "MISS")
			return nil
		}
		// The handler failed (fiber.Error 5xx, or a plain error, which
		// becomes a 500): the last good answer is better than none.
		failed := err != nil || status >= 500
		if fe, ok := err.(*fiber.Error); ok && fe.Code < 500 {
			failed = false
		}
		if failed && !warming {
			if b, ok := o.Cache.GetBytes(ctx, "http:stale:"+k); ok && serve(c, b, "STALE") {
				return nil
			}
		}
		return err
	}
}

// Warmer rebuilds the most requested URLs every Every, up to Top of them,
// one at a time (gentle on a strained database). Run blocks until ctx ends.
type Warmer struct {
	App     *fiber.App
	Cache   *cache.Cache
	Every   time.Duration // default 90s (under the default FreshTTL)
	Top     int           // default 25
	Timeout time.Duration // per request, default 60s
}

func (w Warmer) Run(ctx context.Context) {
	if w.Cache == nil || w.App == nil {
		return
	}
	w.defaults()
	t := time.NewTicker(w.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.round(ctx)
	}
}

func (w *Warmer) defaults() {
	if w.Every <= 0 {
		w.Every = 90 * time.Second
	}
	if w.Top <= 0 {
		w.Top = 25
	}
	if w.Timeout <= 0 {
		w.Timeout = 60 * time.Second
	}
}

func (w Warmer) round(ctx context.Context) {
	w.defaults()
	now := time.Now()
	scores := w.Cache.ZTop(ctx, w.Top, hotKey(now), hotKey(now.Add(-time.Hour)))
	urls := make([]string, 0, len(scores))
	for u, s := range scores {
		if s >= 2 { // asked for more than once: worth keeping ready
			urls = append(urls, u)
		}
	}
	sort.Slice(urls, func(i, j int) bool { return scores[urls[i]] > scores[urls[j]] })
	if len(urls) > w.Top {
		urls = urls[:w.Top]
	}
	warmed, failed := 0, 0
	start := time.Now()
	for _, u := range urls {
		if ctx.Err() != nil {
			return
		}
		req := httptest.NewRequest(fiber.MethodGet, u, nil)
		req.Header.Set(warmHeader, warmToken)
		resp, err := w.App.Test(req, int(w.Timeout.Milliseconds()))
		if err != nil || resp.StatusCode != fiber.StatusOK {
			failed++
		} else {
			warmed++
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}
	if len(urls) > 0 {
		slog.Debug("httpcache: warmed", "ok", warmed, "failed", failed, "took", time.Since(start).Round(time.Millisecond))
	}
}
