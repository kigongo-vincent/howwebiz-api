package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/config"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
)

var (
	TestApp *fiber.App
	Site    *fakeSite
	URL     = "/api/v1"
)

func BASE_URL(v string) string { return fmt.Sprintf("%s/%s", URL, v) }

// fakeSite stands in for howwe.ug, serving pages saved in testdata and
// counting hits so tests can see what the crawl cache avoided.
type fakeSite struct {
	mu        sync.Mutex
	pages     map[string][]byte // "hot100", "audio", "song"
	songHits  atomic.Int64
	listHits  atomic.Int64
	slowUntil atomic.Int64 // unix nanos; listing requests sleep while now < this
}

func (f *fakeSite) set(name string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages[name] = body
}

func (f *fakeSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var key string
	switch {
	case r.URL.Path == "/hot100":
		key = "hot100"
	case strings.HasPrefix(r.URL.Path, "/audio/new"):
		key = "audio"
	case strings.HasPrefix(r.URL.Path, "/song/"):
		f.songHits.Add(1)
		w.Write(f.pages["song"])
		return
	default:
		http.NotFound(w, r)
		return
	}
	f.listHits.Add(1)
	if time.Now().UnixNano() < f.slowUntil.Load() {
		time.Sleep(300 * time.Millisecond)
	}
	w.Write(f.pages[key])
}

func fixture(name string) []byte {
	b, err := os.ReadFile(filepath.Join("..", "modules", "scraper", "testdata", name))
	if err != nil {
		panic(err)
	}
	return b
}

func TestMain(m *testing.M) {
	Site = &fakeSite{pages: map[string][]byte{
		"hot100": fixture("hot100.html"),
		"audio":  fixture("audio_new.html"),
		"song":   fixture("song.html"),
	}}
	srv := httptest.NewServer(Site)
	google := httptest.NewServer(Google)
	dir, _ := os.MkdirTemp("", "howwebiz-test")

	for k, v := range map[string]string{
		"DB_DRIVER": "sqlite", "DB_PATH": filepath.Join(dir, "test.db"), "DB_LOG": "silent",
		"CRAWL_BASE_URL": srv.URL, "CRAWL_REQUEST_DELAY": "0s", "CRAWL_DETAIL_WORKERS": "4",
		"CRAWL_TARGETS": "hot100,audio-new", "CRAWL_PAGES": "1", "CRAWL_DETAIL_MAX_PER_RUN": "0",
		"CRON_ENABLED": "false", "API_KEY": "", "REDIS_ENABLED": "false",
		"GOOGLE_CSE_KEY": "test-key", "GOOGLE_CSE_ENDPOINT": google.URL,
	} {
		os.Setenv(k, v)
	}

	ctx, cancel := context.WithCancel(context.Background())
	TestApp = config.SetupApplication()
	db := config.ConnectDB()
	engine := crawler.NewEngine(ctx, db, crawler.ConfigFromEnv())
	config.RegisterAppRoutes(config.Dependencies{DB: db, App: TestApp, Crawler: engine})

	code := m.Run()
	cancel()
	engine.Wait()
	srv.Close()
	google.Close()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ---- helpers ----

func do(t *testing.T, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := TestApp.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	return res, data
}

func parse[T any](t *testing.T, data []byte) base.APIResponseT[T] {
	t.Helper()
	var out base.APIResponseT[T]
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return out
}

// crawl starts a crawl and waits for it to finish.
func crawl(t *testing.T, opts map[string]any) crawler.CrawlRun {
	t.Helper()
	res, data := do(t, http.MethodPost, BASE_URL("crawl"), opts)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("start crawl: %d %s", res.StatusCode, data)
	}
	id := parse[crawler.CrawlRun](t, data).Data.ID
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, data := do(t, http.MethodGet, BASE_URL(fmt.Sprintf("crawl/runs/%d", id)), nil)
		run := parse[crawler.CrawlRun](t, data).Data
		if run.Status != crawler.StatusRunning {
			if len(run.Errors) > 0 {
				t.Fatalf("run %d errors: %v", id, run.Errors)
			}
			return run
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %d did not finish", id)
	return crawler.CrawlRun{}
}

func TestHealth(t *testing.T) {
	res, _ := do(t, http.MethodGet, BASE_URL("health"), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health: %d", res.StatusCode)
	}
}
