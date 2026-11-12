package catalog

// MemIndex keeps every song's search_text in memory, so finding which
// songs a text query matches never waits on the database: on a database
// busy with crawling, even trigram-indexed lookups took seconds and made
// searches time out. A scan of the whole catalog in Go (one contiguous
// byte buffer, every core) takes tens of milliseconds.
//
// It only selects candidates (see matcher.candidateQueries for the same
// rules); Postgres still scores them and loads the page, by primary key.
//
// Layout: all texts in one []byte arena, entries are small structs without
// pointers, so the garbage collector never walks 700k strings. A base
// index is built at start (in batches, on the background pool) and every
// RebuildEvery; songs added since come in a small delta every DeltaEvery.

import (
	"bytes"
	"cmp"
	"context"
	"log/slog"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
)

type memEntry struct {
	id       uint32
	off      uint32
	n        uint32
	plays    int32
	mix      bool
	hasAudio bool
}

type memSnapshot struct {
	arena   []byte
	entries []memEntry
	maxID   uint32
}

func (s *memSnapshot) text(e memEntry) []byte { return s.arena[e.off : e.off+e.n] }

type MemIndex struct {
	DB           *gorm.DB // the background pool
	Batch        int      // rows per query (default 5000)
	Pause        time.Duration
	DeltaEvery   time.Duration // default 1m
	RebuildEvery time.Duration // default 12h

	base  atomic.Pointer[memSnapshot]
	delta atomic.Pointer[memSnapshot]
}

// Ready: the base index is built (until then, search uses the database).
func (m *MemIndex) Ready() bool { return m != nil && m.base.Load() != nil }

// Size: songs indexed.
func (m *MemIndex) Size() int {
	if !m.Ready() {
		return 0
	}
	n := len(m.base.Load().entries)
	if d := m.delta.Load(); d != nil {
		n += len(d.entries)
	}
	return n
}

type memRow struct {
	ID         uint32
	SearchText string
	Plays      int64
	Kind       string
	HasAudio   bool
}

// load reads songs with id > after into a snapshot (all of them when
// limitRows is 0).
func (m *MemIndex) load(ctx context.Context, after uint32, sizeHint int) (*memSnapshot, error) {
	batch := m.Batch
	if batch <= 0 {
		batch = 5000
	}
	snap := &memSnapshot{maxID: after, arena: make([]byte, 0, sizeHint*120), entries: make([]memEntry, 0, sizeHint)}
	for ctx.Err() == nil {
		var rows []memRow
		err := m.DB.WithContext(ctx).Raw(`SELECT id, search_text, plays, kind, (audio_url <> '') AS has_audio
			FROM catalog_songs WHERE id > ? ORDER BY id LIMIT ?`, snap.maxID, batch).Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			snap.maxID = r.ID
			if r.SearchText == "" {
				continue // not searchable yet (the backfill fills it)
			}
			plays := r.Plays
			if plays > math32 {
				plays = math32
			}
			snap.entries = append(snap.entries, memEntry{id: r.ID, off: uint32(len(snap.arena)), n: uint32(len(r.SearchText)),
				plays: int32(plays), mix: r.Kind == KindMix, hasAudio: r.HasAudio})
			snap.arena = append(snap.arena, r.SearchText...)
		}
		if len(rows) < batch {
			return snap, nil
		}
		if m.Pause > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(m.Pause):
			}
		}
	}
	return nil, ctx.Err()
}

const math32 = 1<<31 - 1

// Build loads the whole catalog into a fresh base index.
func (m *MemIndex) Build(ctx context.Context) error {
	start := time.Now()
	hint := 0
	if old := m.base.Load(); old != nil {
		hint = len(old.entries) + 10000
	}
	snap, err := m.load(ctx, 0, hint)
	if err != nil {
		return err
	}
	m.base.Store(snap)
	m.delta.Store(nil)
	slog.Info("catalog: in-memory search index built", "songs", len(snap.entries),
		"mb", len(snap.arena)>>20, "took", time.Since(start).Round(time.Second))
	return nil
}

// Refresh adds the songs stored since the base was built.
func (m *MemIndex) Refresh(ctx context.Context) error {
	base := m.base.Load()
	if base == nil {
		return nil
	}
	snap, err := m.load(ctx, base.maxID, 1000)
	if err != nil {
		return err
	}
	m.delta.Store(snap)
	return nil
}

// Run builds the index, then keeps it current until ctx ends.
func (m *MemIndex) Run(ctx context.Context) {
	deltaEvery, rebuildEvery := m.DeltaEvery, m.RebuildEvery
	if deltaEvery <= 0 {
		deltaEvery = time.Minute
	}
	if rebuildEvery <= 0 {
		rebuildEvery = 12 * time.Hour
	}
	for ctx.Err() == nil {
		if err := m.Build(ctx); err != nil {
			if ctx.Err() == nil {
				slog.Warn("catalog: in-memory search index build failed, retrying", "err", err)
				select {
				case <-ctx.Done():
				case <-time.After(time.Minute):
				}
			}
			continue
		}
		rebuild := time.After(rebuildEvery)
		tick := time.NewTicker(deltaEvery)
	refresh:
		for {
			select {
			case <-ctx.Done():
				tick.Stop()
				return
			case <-rebuild:
				break refresh
			case <-tick.C:
				if err := m.Refresh(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("catalog: in-memory search index refresh", "err", err)
				}
			}
		}
		tick.Stop()
	}
}

// memHit: one song matching the query, and how well (for pre-ranking).
type memHit struct {
	id     uint32
	words  int // query words it holds
	phrase bool
	all    bool
	prefix bool // short query: its squashed title starts with it
	plays  int32
}

// memScan runs f over every entry of the base and delta, in parallel
// chunks; each worker collects its own hits.
func (m *MemIndex) scan(f func(s *memSnapshot, e memEntry) (memHit, bool)) []memHit {
	var snaps []*memSnapshot
	if b := m.base.Load(); b != nil {
		snaps = append(snaps, b)
	}
	if d := m.delta.Load(); d != nil {
		snaps = append(snaps, d)
	}
	workers := max(1, runtime.GOMAXPROCS(0))
	var mu sync.Mutex
	var out []memHit
	var wg sync.WaitGroup
	for _, s := range snaps {
		chunk := (len(s.entries) + workers - 1) / workers
		for w := 0; w < workers; w++ {
			lo, hi := w*chunk, min((w+1)*chunk, len(s.entries))
			if lo >= hi {
				continue
			}
			wg.Go(func() {
				var local []memHit
				for _, e := range s.entries[lo:hi] {
					if h, ok := f(s, e); ok {
						local = append(local, h)
					}
				}
				mu.Lock()
				out = append(out, local...)
				mu.Unlock()
			})
		}
	}
	wg.Wait()
	return out
}

// Candidates selects the songs a relevance search should score: the same
// lookups as matcher.candidateQueries (phrase or squashed, every word,
// short-query title prefix, any word), each bounded, but pre-ranked by how
// many words match and popularity instead of taken in arbitrary order.
// kind: song | mix | all; audioOnly drops songs without audio.
func (m *MemIndex) Candidates(words []string, kind string, audioOnly bool) []uint {
	if len(words) == 0 {
		return nil
	}
	ws := make([][]byte, 0, len(words))
	short := true
	for _, w := range words {
		short = short && utf8.RuneCountInString(w) < 3
	}
	for _, w := range words {
		// as in SQL: words under three letters only count when the query
		// has nothing longer
		if short || utf8.RuneCountInString(w) >= 3 {
			ws = append(ws, []byte(w))
		}
	}
	phrase := []byte(joinWords(words, " "))
	squashed := []byte(joinWords(words, ""))
	multi := len(words) > 1
	hits := m.scan(func(s *memSnapshot, e memEntry) (memHit, bool) {
		if (kind == KindSong && e.mix) || (kind == KindMix && !e.mix) || (audioOnly && !e.hasAudio) {
			return memHit{}, false
		}
		t := s.text(e)
		h := memHit{id: e.id, plays: e.plays}
		for _, w := range ws {
			if bytes.Contains(t, w) {
				h.words++
			}
		}
		if multi && (bytes.Contains(t, phrase) || bytes.Contains(t, squashed)) {
			h.phrase = true
		}
		if short && bytes.HasPrefix(t, squashed) {
			h.prefix = true
		}
		h.all = h.words == len(ws) && len(ws) > 0
		return h, h.words > 0 || h.phrase || h.prefix
	})
	better := func(a, b memHit) int { // > 0: a ranks above b
		if c := cmp.Compare(a.words, b.words); c != 0 {
			return c
		}
		if c := cmp.Compare(a.plays, b.plays); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	}
	pick := func(keep func(memHit) bool, n int) []uint32 {
		top := topK(hits, keep, n, better)
		out := make([]uint32, len(top))
		for i, h := range top {
			out[i] = h.id
		}
		return out
	}
	seen := map[uint32]bool{}
	var ids []uint
	add := func(list []uint32) {
		for _, id := range list {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, uint(id))
			}
		}
	}
	if multi {
		add(pick(func(h memHit) bool { return h.phrase }, 300))
		add(pick(func(h memHit) bool { return h.all }, candidateCap))
	}
	if short {
		add(pick(func(h memHit) bool { return h.prefix }, 300))
	}
	add(pick(func(h memHit) bool { return h.words > 0 }, candidateCap))
	return ids
}

// Count: songs holding w as typed, up to limit (the "is this word rare?"
// test of typo matching).
func (m *MemIndex) Count(w string, limit int) int {
	b := []byte(w)
	var n atomic.Int64
	m.scan(func(s *memSnapshot, e memEntry) (memHit, bool) {
		if n.Load() < int64(limit) && bytes.Contains(s.text(e), b) {
			n.Add(1)
		}
		return memHit{}, false
	})
	return int(min(n.Load(), int64(limit)))
}

func joinWords(words []string, sep string) string {
	var b bytes.Buffer
	for i, w := range words {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(w)
	}
	return b.String()
}

// AllWords: songs holding every word (an explicit sort lists only those),
// most played first, at most limit.
func (m *MemIndex) AllWords(words []string, kind string, audioOnly bool, limit int) []uint {
	ws := make([][]byte, len(words))
	for i, w := range words {
		ws[i] = []byte(w)
	}
	hits := m.scan(func(s *memSnapshot, e memEntry) (memHit, bool) {
		if (kind == KindSong && e.mix) || (kind == KindMix && !e.mix) || (audioOnly && !e.hasAudio) {
			return memHit{}, false
		}
		t := s.text(e)
		for _, w := range ws {
			if !bytes.Contains(t, w) {
				return memHit{}, false
			}
		}
		return memHit{id: e.id, plays: e.plays}, true
	})
	top := topK(hits, func(memHit) bool { return true }, limit, func(a, b memHit) int {
		if c := cmp.Compare(a.plays, b.plays); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})
	out := make([]uint, len(top))
	for i, h := range top {
		out[i] = uint(h.id)
	}
	return out
}

// topK returns the n best hits passing keep, best first, in O(len log n)
// (a bounded min-heap) instead of sorting every hit: a common word can
// match hundreds of thousands of songs.
func topK(hits []memHit, keep func(memHit) bool, n int, better func(a, b memHit) int) []memHit {
	if n <= 0 {
		return nil
	}
	heap := make([]memHit, 0, n) // heap[0] is the worst kept
	less := func(i, j int) bool { return better(heap[i], heap[j]) < 0 }
	down := func(i int) {
		for {
			l, r, m := 2*i+1, 2*i+2, i
			if l < len(heap) && less(l, m) {
				m = l
			}
			if r < len(heap) && less(r, m) {
				m = r
			}
			if m == i {
				return
			}
			heap[i], heap[m] = heap[m], heap[i]
			i = m
		}
	}
	for _, h := range hits {
		if !keep(h) {
			continue
		}
		if len(heap) < n {
			heap = append(heap, h)
			for i := len(heap) - 1; i > 0; { // up
				p := (i - 1) / 2
				if !less(i, p) {
					break
				}
				heap[i], heap[p] = heap[p], heap[i]
				i = p
			}
			continue
		}
		if better(h, heap[0]) > 0 {
			heap[0] = h
			down(0)
		}
	}
	slices.SortFunc(heap, func(a, b memHit) int { return better(b, a) })
	return heap
}
