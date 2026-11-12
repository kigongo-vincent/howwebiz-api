**HOWWEBIZ API**

Crawls [howwe.ug](https://www.howwe.ug): the Hot 100 chart, the `/audio/*` listings, and the **entire song catalog** (~120k songs with mp3 links). It stores everything in Postgres and serves it as paginated JSON with search. Crawls run on demand through the API, always once at startup for every source, and once a day from an internal scheduler.

Stack: Go · Fiber · GORM · Postgres (SQLite also supported for tests/quick local runs).

## Run

```sh
cp .env.example .env
docker compose up -d      # Postgres only
go run .                  # API on :3000
```

## API docs (Swagger)
With the server running:

- **Swagger UI:** http://localhost:3000/docs (try requests in the browser; set `X-API-Key` under *Authorize* when `API_KEY` is set)
- **OpenAPI 3 spec:** `/docs/openapi.json` and `/docs/openapi.yaml`

The spec is hand-written in `modules/docs/openapi.yaml` and embedded in the binary. `test/docs_test.go` keeps it honest. It fails if a route exists that the spec doesn't document, if the spec documents a route that doesn't exist, if a `$ref` doesn't resolve, or if a `{param}` isn't declared. When you add or change an endpoint, update `openapi.yaml` in the same change.

## Aggregated catalog: `GET /api/v1/songs`

This is the one entry point for songs. Every provider's copy of a song is merged into **one entry**, with all copies listed under `sources` (best first).

- **Matching:** normalized *primary artist + title*. Case, accents and punctuation are ignored, and so is packaging noise ("(Official Audio)", "(MP3_160K)", "[Video]", "Prod. …") and "ft./feat." credits, which move to `featured`. Versions stay separate: *Nassanga*, *Nassanga (Remix)* and *Nassanga (Live)* are three songs. "Kalifah AgaNaga ft. Eddy Kenzo" on howwe and "Nassanga (Remix) (Official Audio)" elsewhere are one entry.
- **Only playable songs:** the catalog stores and serves only songs with an audio URL. Tracks without one (listing cards, search hits) are rejected at ingest (`noAudio` in the stats). The provider's crawler then fetches the song page that carries the mp3. Data stored before this rule is removed at server start, or on demand with `POST /api/v1/songs/cleanup`. Provider views (`/howwe/songs`, `/momuziki/songs`, `/audiocom/tracks`) also default to `hasAudio=true`; their tables keep audio-less rows only as crawl state (songs whose mp3 is still to be fetched), and `hasAudio=false` shows those.
- **Display fields** (title, cover, audio, genre) come from the highest-priority provider that has them (`CATALOG_PROVIDER_PRIORITY`). `plays` is summed across providers.
- **Live search:** on page 1 of `?q=` (`live=auto`), every provider's own search runs in parallel, with a per-provider timeout and a 15-minute cache per provider+query. Results are ingested, and the answer comes from the merged catalog. `meta.live` reports each provider as `ok` / `cached` / `skipped` (not configured, not connected, paused, or disallowed by robots.txt) / `error` / `timeout`. Use `live=false` for catalog-only queries.
- **Filters:** `q`, `artist`, `genre`, `provider`, `hasAudio`. `sort=relevance|recent|plays|title`. Relevance ranks exact title, then title prefix, then primary artist, then the rest, with ties broken by provider count and plays.
- `GET /api/v1/songs/{id}` returns one entry with every copy. `GET /api/v1/providers` lists every provider and how much of the catalog it backs. `POST /api/v1/songs/reindex` indexes now.
- **Discovery.** `GET /api/v1/songs?sort=discover` is a shuffled feed: popular, widely carried, fresh and (with `?region=ug|ke|tz|rw|ng|gh|za|africa|global`) in-region songs come up more often, artists spread out. Combines with `genre`, `artist`, `type`. `GET /api/v1/songs/{id}/next` is "up next": similar by genre, collaborators, region, era, reach, length and title themes, never another version of the same song, the same artist at most twice (`?limit=` `?region=` `?exclude=` played ids). Both are weighted at random; send back `meta.seed` as `?seed=` to page one feed or replay a list.
- **Caching.** Read endpoints are cached in Redis (~2 min) and rebuilt in the background while popular. When the database is slow or failing, the last good answer (up to 24h) is served instead of an error. `X-Cache: HIT | MISS | STALE` says which.
- Provider-specific endpoints remain for crawl control and provider views. howwe's own `/songs` and `/search` moved to `/api/v1/howwe/songs` and `/api/v1/howwe/search`.

### Architecture

Dependencies point inward. The catalog never imports a provider.

```
modules/catalog/           domain + use cases: Track, Song/Source, matching (normalize.go),
                           Ingester (merge, sparse data never erases rich data), Indexer, SearchService,
                           ports: Store (provider has its own tables) and Searcher (live search)
modules/providers/
  sitecrawl/               generic site/API crawler (the momuziki pattern), driven by optional capabilities:
                           Lister (newest-first pages) · Prober (song pages by id) · Sitemapper (lastmod) · Searchable
                           · Feeder (API feeds with per-feed cursors) · APISearcher · Enabler (needs config, e.g. a key)
  pearltunes/              parsers only
  ugamusic/                parsers only
  audius/                  API client only (Feeder + APISearcher)
  internetarchive/         API client only (Feeder + APISearcher)
modules/{song,search,momuziki,audiocom}/catalog_adapter.go   existing providers → catalog ports
config/wiring.go           composition root (also used by tests)
```

For sites with song pages by id (pearltunes), the listing is discovery only, since its cards carry no mp3: page 1 gives the newest id and the sweep fetches song pages. Live search results without audio are enriched by fetching their song pages (`<NAME>_SEARCH_ENRICH`, default 5). Enrichment stops before the search deadline and returns what it has.

Adding a site means writing a parser package that implements the capabilities its robots.txt allows, and adding it to `config/wiring.go`. Crawling, resuming, incremental sync, guards, ingest, live search and the shared endpoints (`/api/v1/providers/{name}/status|crawl|guard/reset`) come from `sitecrawl`.

### Full-track APIs, worldwide (2026-11-10)

These carry **full-length tracks**, not 30-second previews, from artists in every country. They go through the same `sitecrawl` crawler as a **Feeder**: each feed (a genre or a collection) has its own resumable cursor in `sitecrawl_feeds`, feeds take turns one page at a time (so a small `maxPages` still reaches every feed), and requests go through webguard (delay, 429/Retry-After, circuit breaker).

| Provider | Catalog | Feeds | Key | Terms |
|---|---|---|---|---|
| **audius** ([audius.co](https://audius.co)) | open artist uploads, millions of tracks | newest first per genre (`AUDIUS_GENRES`, + `all`); the search index stops at 10k per feed, so later runs read from the top until a page adds nothing new | none (`AUDIUS_APP_NAME` is sent) | Audius-hosted, with its own takedown process. Stream-gated tracks (premium/follow-gated: preview only) are skipped. `AUDIUS_VERIFIED_ONLY=true` keeps verified artists only (~5–10% of uploads, fewer re-uploads of others' music) |
| **internetarchive** ([archive.org](https://archive.org)) | `netlabels` (~80k releases) and `etree`, the Live Music Archive (~300k concerts) (`INTERNETARCHIVE_COLLECTIONS`) | per collection, oldest added first (tail feed; the watermark re-anchors before advancedsearch's 10k cap); one `/metadata` request per item, one track per mp3 | none | per-item licences (mostly CC for netlabels; etree artists allow non-commercial distribution). Live recordings get a "(Live)" suffix so they stay separate from studio versions. Daily budget `INTERNETARCHIVE_MAX_PAGES=200` pages × 25 items |

`audioUrl` is always a stable full-track URL: `api.audius.co/v1/tracks/{id}/stream` (302 to a content node, Range supported), `archive.org/download/{item}/{file}.mp3`.

Not added: Spotify, Apple Music, Deezer and YouTube Music APIs give 30-second previews or no audio at all, and their terms forbid storing or redistributing audio. SoundCloud's API is closed to new apps. Free Music Archive has no public API any more. ccMixter is mostly remixes and stems. The `georgeblood`/`78rpm` (Great 78) collections are excluded because record labels have contested their copyright.

```sh
curl -X POST localhost:3000/api/v1/providers/audius/crawl -H 'Content-Type: application/json' -d '{"maxPages":50}'
curl localhost:3000/api/v1/providers/audius/status   # feeds: cursor, walkDoneAt, pages, tracks per feed
```

### Sites researched (2026-11-09)

| Site | Result |
|---|---|
| [pearltunes.com](https://pearltunes.com) | ✅ added. Artist-submitted ("Send your music"), robots.txt allows all. ~15.4k songs: newest-first listing (`/songs/new/page/N`), song pages resolve by id and embed `var pt` (the song + the artist's others, with mp3), `/search?q=` |
| [ugamusic.net](https://ugamusic.net) | ✅ added, **sitemap only**. robots.txt disallows `/*?*page=` and `/search?` (and `/storage/`) and sets Crawl-delay 1. So it's `/sitemap/songs` (27.6k pages, `lastmod`) → `/songs/{slug}` pages (the song + related + recommended, with mp3). No live search |
| [mdundo.com](https://mdundo.com) | ❌ terms grant a "personal, non-commercial" licence and forbid "making available to the public" |
| feezah.com | ❌ the domain is now a drinks shop |
| musiclibrary-ug.pages.dev | ❌ "on Pause" |
| djerycom.ug | ❌ no robots.txt or terms; a personal DJ site re-hosting others' music |

## Layout

```
app.go                 entrypoint: setup → db → crawler engine + scheduler → routes → graceful shutdown
config/                Dependencies, SetupApplication, ConnectDB (+ AutoMigrate), RegisterAppRoutes
modules/
  base/                RouteRegister, APIResponse, pagination (?page=&limit=), env helpers
  scraper/             polite HTTP client, HTML parsers, Fingerprint (testdata/ = saved real pages)
  song/                Song model, /songs routes, cache-aware upsert/detail repository
  chart/               list registry, Snapshot/Entry models, /lists + /charts routes
  genre/               Genre model (howwe's /genres index), /genres routes
  crawler/             Runner (run lifecycle, one per source), Engine (howwe list + catalog crawls), Scheduler, /crawl routes
  search/              /search: local catalog (ranked + fuzzy) or howwe.ug's own search engine
  audiocom/            audio.com source: OAuth (PKCE, dynamic registration), rate-limited API client, feeds crawl, website crawl, routes
  momuziki/            momuziki.com source: HTML parsers (testdata = real pages), categories walk, id sweep, search, routes
  webguard/            polite fetcher shared by HTML sources: robots.txt, no cookies, block counter + circuit breaker
  docs/                openapi.yaml (embedded) + Swagger UI at /docs
  catalog/             aggregated catalog (see "Aggregated catalog")
  providers/           sitecrawl (generic HTML-site crawler) + pearltunes, ugamusic
test/                  API + crawl-cache integration tests against a fake howwe.ug
```

## Full catalog

`POST /api/v1/crawl/catalog` advances a **resumable** crawl of every song on howwe.ug. Its position (phase, genre, page, id) is stored in the DB, so it survives restarts and picks up where the last run stopped.

The source is [`/genres`](https://www.howwe.ug/genres): 42 genres reporting **125,135 songs**, each paginated at `/genres/{Name}/page/N` with 36 songs per page. That is more than `/audio/new` (~120k), and it includes old songs that no other listing shows. It also gives each song its **genre**, which song pages don't show.

1. **genres**: walks every genre in name order, each to its last page. howwe.ug repeats the last page instead of returning a 404, so the end is a repeated page or a missing "next" link.
2. **sweep**: a safety net that probes every song id from 1 to the highest known id that no genre listed (e.g. songs with no genre). Real songs are stored with their real slug and artist id, read from the song page. Empty ids are remembered and re-checked only after 30 days.
3. **synced**: each later run walks `/audio/new` (newest first) from page 1 and **stops at the first page with nothing new**, sweeps only ids above the last maximum, and re-walks all genres every `CATALOG_GENRE_REFRESH` (default 7 days). During a re-walk, unchanged pages cost a request but no writes (page fingerprints).

Every run also fetches song pages that are due (mp3 URL, album, rating), songs never fetched first. Per-run budgets: `maxPages`, `maxProbes`, `maxDetails` (0 = unlimited). `refreshGenres: true` starts a genre re-walk now, and `reset: true` starts over. The daily cron runs one catalog step after the list crawl.

```sh
# everything, in one go (long: see below)
curl -X POST localhost:3000/api/v1/crawl/catalog -H 'Content-Type: application/json' \
  -d '{"maxPages":0,"maxProbes":0,"maxDetails":0}'
curl localhost:3000/api/v1/crawl/catalog   # phase, current genre/page, songs, coverage vs howwe's 125k
curl localhost:3000/api/v1/genres          # per genre: howwe's count vs songs stored
```

**Time it takes:** requests are paced by `CRAWL_REQUEST_DELAY` (500ms ≈ 2 req/s). The genre walk is about 3,500 pages, roughly 30 minutes. Song pages for ~125k songs take about 17 hours. Lowering the delay speeds this up but puts more load on howwe.ug. With the default daily budgets (10k song pages per day), mp3 links for the whole catalog fill in over about 13 days.

## audio.com source

audio.com is a second source, kept in its own tables (`audiocom_*`) with its own crawl runner, so a long audio.com crawl never blocks the howwe.ug crawls.

**Why the API and not the website:** every audio.com page, including `/search`, `/genres` and the sitemap, is behind a Cloudflare bot challenge (`403 cf-mitigated: challenge`). This project doesn't try to get around that. audio.com publishes an [official API](https://audio.com/public-api) ([OpenAPI](https://api.audio.com/v1/openapi.json)), and its `GET /v1/search` covers what the website pages show:

| Website | API equivalent |
|---|---|
| `/search?search_text=bnxn&sort=relevance` | `query=bnxn` (relevance-ranked) |
| `/genres/pop` + pagination | `query=#pop` (exact tag match), `page=N` |
| everything | empty `query` = all listed audio, **newest first** |

Limits: 10 results per page and 60 requests per minute per user (this source uses 55/min by default). Each track includes `playUrl` (a streamable mp3), duration, tags, category, license and cover.

**Connect (once):** the API requires OAuth 2.1 with PKCE as a real audio.com user. There are no app-only keys.

```sh
curl localhost:3000/api/v1/audiocom/connect          # → {"authorizeUrl": ...}; open it in a browser, sign in
# (or open http://localhost:3000/api/v1/audiocom/connect?redirect=true directly)
curl localhost:3000/api/v1/audiocom/status           # connected: true
```

The client registers itself with audio.com on first use (dynamic client registration). `AUDIOCOM_REDIRECT_URL` must point at this server's `/api/v1/audiocom/callback`, at an address your browser can reach. Tokens are stored in `audiocom_auth`, refreshed automatically (refresh tokens rotate), and never returned by the API.

**Crawl:** `POST /api/v1/audiocom/crawl` with `{"tags":["pop","hiphop"],"maxPages":0,"skipAll":false,"reset":false}`.

- **all** feed: the first run walks every page (resumable from the stored page). Later runs start at page 1 and **stop at the first page with nothing new or changed**, because the feed is newest-first.
- **#tag** feeds (the genres equivalent): walked fully and resumably, then re-walked every `AUDIOCOM_TAG_REFRESH`. They're ordered by relevance, so they can't stop early.
- Tracks are fingerprinted, so unchanged tracks cost no writes. The client retries 429 responses (honouring `Retry-After`) and 5xx responses, and refreshes the token once on 401.
- The daily cron runs an audio.com crawl next to the howwe.ug crawl when connected (`AUDIOCOM_DAILY`).
- How long "all" takes depends on audio.com's size, which the API doesn't report. At 55 pages/min that's about 550 tracks/min, or 33k per hour.

| Method | Path | |
|---|---|---|
| `GET` | `/api/v1/audiocom/connect` | start OAuth (`?redirect=true` to jump to sign-in) |
| `GET` | `/api/v1/audiocom/callback` | OAuth redirect target (state + PKCE checked) |
| `POST` | `/api/v1/audiocom/disconnect` | forget tokens |
| `GET` | `/api/v1/audiocom/status` | connected?, feeds and their positions, track counts, last run |
| `POST` | `/api/v1/audiocom/crawl` | start a crawl (202 / 409 running / 412 not connected) |
| `GET` | `/api/v1/audiocom/search` | live official search: `?q=` (text or `#tag`) `?types=audio,author,collection` `?page=`. Cached, and audio hits are stored |
| `GET` | `/api/v1/audiocom/tracks` | stored tracks: `?q=` `?tag=` `?category=` `?username=` `?hasAudio=` `?sort=recent\|title\|duration` |
| `GET` | `/api/v1/audiocom/tracks/:id` | |

`POST` endpoints and `/connect` require `X-API-Key` when `API_KEY` is set.

### Website crawl (HTML) with a block counter

The website crawler fetches `/genres`, then each `/genres/{name}` and `/genres/{name}?page=N` until a page adds nothing, plus `/search?search_text=…&sort=relevance`. Tracks go into the same `audiocom_tracks` table. It shares a runner with the API crawl, so only one audio.com crawl runs at a time.

**It makes honest requests and stops when blocked. It never tries to get past a block.**

- One fixed User-Agent, no cookie jar (challenge clearance cookies are never sent back), one request at a time, `AUDIOCOM_WEB_DELAY` between requests.
- **robots.txt is obeyed** for every URL. audio.com's robots.txt allows `/search` and `?page=` and disallows other query strings.
- **Block detection:** `cf-mitigated` header, challenge pages ("Just a moment...", `challenge-platform`), 403, 429, and Cloudflare 503.
- **Block counter** (persisted in `audiocom_web_health`):
  - each block → exponential backoff (`AUDIOCOM_WEB_BLOCK_BACKOFF`, doubling), then the same URL is retried with the same headers
  - `MAX_CONSECUTIVE_BLOCKS` in a row, or `MAX_BLOCKS_PER_RUN` in one run → **the circuit opens**: the run stops, and every web request (crawl, search, the daily job) is refused until the cooldown ends. The cooldown doubles each time the circuit re-opens, up to `AUDIOCOM_WEB_MAX_COOLDOWN`.
  - a page that loads resets the consecutive count
- **Layout guard:** after `AUDIOCOM_WEB_MAX_UNRECOGNIZED` pages in a row that load without recognizable track data, the run stops instead of walking everything for nothing.

The extractor reads the data Next.js embeds in the page (RSC flight data, `__NEXT_DATA__`, JSON-LD), not CSS classes. It has only been tested on synthetic pages, because every real audio.com page answered with a Cloudflare challenge during development. If the real pages differ, the layout guard stops the run, and the error shows the page title and which embedded formats were found.

| Method | Path | |
|---|---|---|
| `GET` | `/api/v1/audiocom/web/status` | paused?, block counter (consecutive/total/trips, last block reason and URL), guard settings, genre feeds |
| `POST` | `/api/v1/audiocom/web/crawl` | `{"genres":["pop"],"maxPages":0,"reset":false}` (genres empty = all). 202 / 409 / **503 while paused** |
| `POST` | `/api/v1/audiocom/web/reset` | clear the pause (operator). Blocks keep being counted |
| `GET` | `/api/v1/audiocom/web/search` | `?q=&page=`. **503 while paused**, pointing to `/api/v1/audiocom/search` (official API) |

## momuziki.com source

Crawled from its HTML pages. robots.txt allows general crawlers (`Allow: /`) and sets `Content-Signal: search=yes, ai-train=no`. **Don't use this data to train AI models.** Search and catalog use is what the site permits.

| Page | Gives |
|---|---|
| `categories.php` | 18 categories (id, name, image) |
| `category.php?id=N&page=P` | 30 songs per page, **newest first**: id, title, artist, thumbnail, **direct mp3 URL**. Past the last page the list is empty |
| `song.php?id=N` | play count, full-size cover, mp3. An unknown id returns an empty shell |
| `search.php?query=…` | up to 40 matches, same row format |

**Getting everything** (`POST /api/v1/momuziki/crawl`, resumable, budgeted):

1. **categories**: walks every category to its last page.
2. **sweep**: probes `song.php?id=N` for every id from 1 to the highest known id that no category listed, then keeps going **above** the highest id until `MOMUZIKI_PROBE_AHEAD` misses in a row. New uploads can appear before they're categorized (e.g. 5992 was live while category pages topped out at 5991).
3. **synced**: each later run reads every category from page 1 and stops at the first page with nothing new, re-walks all categories every `MOMUZIKI_CATEGORY_REFRESH`, and probes above the highest id.

Every run also fetches due song pages for play counts and covers. A song page is revisited after `MOMUZIKI_DETAIL_TTL`, and the interval doubles (up to 8×) while the page is unchanged. Unchanged listing pages cost a request but no writes (page fingerprints).

Requests go through the same **webguard** block counter as audio.com, with its own health row: honest User-Agent, no cookies, robots.txt obeyed, backoff on blocks, and a circuit breaker after `MOMUZIKI_MAX_CONSECUTIVE_BLOCKS`. Live search falls back to the local catalog while the guard is paused. If 5 listed songs in a row have empty pages, the run stops, since that suggests the layout changed. The site is about 6k songs, so a full first crawl at 1 request per second is roughly 200 listing pages plus about 6k song pages, around 2 hours.

| Method | Path | |
|---|---|---|
| `POST` | `/api/v1/momuziki/crawl` | `{"maxPages","maxProbes","maxDetails","refreshCategories","reset"}` (0 = unlimited). 202 / 409 / 503 while paused |
| `GET` | `/api/v1/momuziki/status` | phase/cursor, songs, songs with audio, details due, highest id, guard health, last run |
| `POST` | `/api/v1/momuziki/guard/reset` | clear a pause |
| `GET` | `/api/v1/momuziki/search` | `?q=`. Live momuziki search (site's order, enriched with stored plays/covers); `?source=local` searches the stored catalog |
| `GET` | `/api/v1/momuziki/songs` | `?q=` `?categoryId=` `?artist=` `?hasAudio=` `?sort=recent\|title\|plays` |
| `GET` | `/api/v1/momuziki/songs/:id` | |
| `GET` | `/api/v1/momuziki/categories` | categories with stored song counts (`meta.uncategorized` = songs found only by the sweep) |
| `GET` | `/api/v1/momuziki/categories/:id/songs` | same filters as `/songs` |

## Search

`GET /api/v1/search?q=...&source=auto|local|howwe`

- **local** (the default when no Google key is set): searches the crawled catalog. Every word must match the title or the artist. Exact and prefix title matches rank first, then popularity (votes). If nothing matches, it falls back to **fuzzy** trigram matching on Postgres (`pg_trgm`), so "nasanga" finds "Nassanga (Remix)" and "ykee bender" finds Ykee Benda. Once the catalog crawl is complete, this covers everything on howwe.ug.
- **howwe**: howwe.ug has no server-side search. Its `/search` page embeds a Google Programmable Search Engine (`cx=76ecd7f17cc7e4f1b`). This source queries that same engine through Google's Custom Search JSON API, so it needs `GOOGLE_CSE_KEY`. Song hits are joined to the catalog, and songs the catalog lacks are added (the next crawl fetches their mp3s). Responses are cached for `SEARCH_CACHE_TTL`. ⚠️ Google has closed this API to new customers and **discontinues it on 2027-01-01**. Google also limits results to 10 per page and 100 per query.

## How the crawl cache works

howwe.ug sends `Cache-Control: no-store` with no ETag or Last-Modified, and its HTML changes on every request, so HTTP caching can't be used. Instead, every layer compares a **fingerprint of the parsed data** and skips work or writes when nothing changed:

| Layer | What is cached | Saving |
|---|---|---|
| Listing page | `page_fingerprints`: hash per URL | unchanged page → no song upserts at all |
| Newest-first lists (`audio-new`) | page 1 unchanged ⇒ nothing new was added | deeper pages are **not fetched** |
| Songs | `listing_hash` per song (title/artist/slug) | only changed rows are written; inserts batched |
| Covers | prefers song covers over artist photos | no flip-flopping between listings |
| List snapshots | content hash of order + chart stats | identical list → only `lastSeenAt` moves; table grows only on real changes |
| Song pages | `detail_hash` + `next_detail_check_at` | fetched only when due; TTL doubles (up to 8×) while unchanged |
| Per-run cap | `CRAWL_DETAIL_MAX_PER_RUN` | big first crawls spread over several runs |
| API reads | ETag middleware | repeat requests get `304` between crawls |

Each run records what every layer saved in `stats`. Live example, two identical crawls of `hot100` + 2 pages of `audio-new`:

```
run 1: pagesFetched=3  songsInserted=172 snapshotsCreated=2 detailsFetched=172
run 2: pagesFetched=2  pagesSkipped=1   pagesUnchanged=2   snapshotsReused=2 detailsNotDue=172   (2 requests instead of 175)
```

`"force": true` skips every cache layer for one run.

## API

Responses use `{ "data": ..., "msg": "...", "pagination": { "page", "limit", "total", "totalPages", "hasMore" }, "meta": ... }`.
Lists are paginated with `?page=` (from 1) and `?limit=` (1–100, default 20).

| Method | Path | Notes |
|---|---|---|
| `POST` | `/api/v1/crawl` | manual crawl; optional body `{"targets":[...],"pages":3,"fetchDetails":true,"force":false}`. 202 / 409 if one is running / 400. Needs `X-API-Key` if `API_KEY` is set |
| `GET` | `/api/v1/crawl/status` | running?, next scheduled time, targets, defaults |
| `POST` | `/api/v1/crawl/catalog` | next step of the full-catalog crawl; body `{"maxPages","maxProbes","maxDetails","refreshGenres","reset"}` (0 = unlimited) |
| `GET` | `/api/v1/crawl/catalog` | catalog progress: phase, current genre/page, song/audio/backlog counts, coverage, last run |
| `GET` | `/api/v1/crawl/runs` | history (`?status=`), newest first; long runs update their `stats` every 15s |
| `GET` | `/api/v1/crawl/runs/:id` | one run with stats + errors |
| `GET` | `/api/v1/search` | `?q=` `?source=auto\|local\|howwe`; see Search |
| `GET` | `/api/v1/songs` | `?q=` `?artistId=` `?genre=` `?hasAudio=true\|false` `?sort=recent\|title\|rating\|votes` |
| `GET` | `/api/v1/genres` | all genres: `siteCount` (howwe) vs `songs` (stored) |
| `GET` | `/api/v1/genres/:name/songs` | songs of a genre; same filters/sort/paging as `/songs` |
| `GET` | `/api/v1/songs/:id` | |
| `GET` | `/api/v1/lists` | each list with its current snapshot |
| `GET` | `/api/v1/lists/:list` | entries + songs; `?date=YYYY-MM-DD` = the list as of that day |
| `GET` | `/api/v1/lists/:list/snapshots` | distinct versions of a list |
| `GET` | `/api/v1/charts/hot100` | shortcut for `/lists/hot100` |
| `GET` | `/api/v1/health` | checks the DB too |

Lists: `hot100`, `audio-new`, `audio-day`, `audio-week`, `audio-month`, `audio-year`, `audio-alltime`.
Hot 100 entries add `trend` (`up`/`down`/`steady`/`new`; `new` also covers re-entries), `lastDay`, `lastWeek`, `peak`, `weeksOnChart`.

## Configuration

All settings are in `.env.example`. The main ones:

- `CRON_AT` / `CRON_TZ`: time of the daily crawl (default `03:00`, `Africa/Kampala`). `CRON_ENABLED=false` turns it off.
- `CRAWL_TARGETS` / `CRAWL_PAGES`: what the daily crawl fetches and the default for manual crawls.
- `CRAWL_REQUEST_DELAY`: minimum gap between requests to howwe.ug.
- `CRAWL_DETAIL_TTL`: base revisit interval for song pages.
- `CATALOG_*`: per-run budgets for the catalog crawl, whether the daily cron advances it, and how often genres are re-walked.
- `GOOGLE_CSE_KEY`: turns on `source=howwe` search.
- `DB_DRIVER=sqlite`: run without Postgres.

## Tests

```sh
go test ./...
```

Integration tests run on a temp SQLite DB against a fake howwe.ug, and assert hit counts so they prove the cache is working.
