package catalog

import (
	"strings"
	"unicode/utf8"

	"gorm.io/gorm/clause"
)

// matcher builds the SQL for a folded multi-word text query.
//
// Each word matches a song when it is:
//   - a substring of search_text (which also holds every field squashed, so
//     "yomwana" finds "Dole Y'omwana"), or
//   - on Postgres with pg_trgm, close enough to a word in it: pg_trgm's
//     word_similarity >= pg_trgm.word_similarity_threshold (0.6 by default),
//     via the <% operator the search_text GIN index answers. "ssebatta"
//     finds "Ssebata"; a one-letter slip in a short word doesn't, which keeps
//     the noise down.
//
// Typo matching applies only to words that are rare as typed (fuzzy set
// by the caller, see SearchService.rareWords): a word thousands of songs
// already hold is spelled right, and its near-misses are only noise (and the
// costliest part of a query).
//
// Scores per word: a whole word 1.0, the start of a word 0.85, inside a
// word 0.5 (0.25 for fragments under four letters: "bby" in "chubby"), a
// typo match 0.75 x its similarity (0.45-0.75, below any whole-word hit).
// The whole query found as a phrase, or squashed, adds 2: the song typed in
// full comes first.
type matcher struct {
	words   []string
	indexed bool // search_text is populated
	// fuzzy: per rare word, the ids of the songs closest to it (computed
	// once per word by SearchService.fuzzyIDs, then cached)
	fuzzy map[string][]uint
}

// fuzzyMinRunes: shorter words share too many trigrams with unrelated words.
const fuzzyMinRunes = 4

// fuzzyMaxSongs caps the songs one word's typo matching adds.
const fuzzyMaxSongs = 200

func (m matcher) exact(w string) clause.Expr {
	like := "%" + likeEscape(w) + "%"
	if m.indexed {
		return clause.Expr{SQL: `search_text LIKE ? ESCAPE '\'`, Vars: []any{like}}
	}
	// before search_text is filled: the display fields, folded by LOWER only
	return clause.Expr{SQL: `(key LIKE ? ESCAPE '\' OR LOWER(title) LIKE ? ESCAPE '\' OR LOWER(artist) LIKE ? ESCAPE '\' OR LOWER(CAST(featured AS TEXT)) LIKE ? ESCAPE '\')`,
		Vars: []any{like, like, like, like}}
}

func (m matcher) canFuzz(w string) bool { return len(m.fuzzy[w]) > 0 }

// branches are the separately indexed lookups that select songs for w.
func (m matcher) branches(w string) []clause.Expr {
	out := []clause.Expr{{SQL: `SELECT id FROM catalog_songs WHERE search_text LIKE ? ESCAPE '\'`, Vars: []any{"%" + likeEscape(w) + "%"}}}
	if m.canFuzz(w) {
		out = append(out, clause.Expr{SQL: `SELECT id FROM catalog_songs WHERE id IN (?)`, Vars: []any{m.fuzzy[w]}})
	}
	return out
}

// union selects the songs any of the branches select, each branch on its
// own index: ORed into one WHERE, Postgres gave up the indexes and computed
// trigram similarity on every row (5.8s on 150k rows).
func union(branches []clause.Expr) clause.Expr {
	e := join(branches, " UNION ")
	return clause.Expr{SQL: "id IN " + e.SQL, Vars: e.Vars}
}

func (m matcher) word(w string) clause.Expr {
	if !m.canFuzz(w) {
		return m.exact(w)
	}
	return union(m.branches(w))
}

func join(exprs []clause.Expr, op string) clause.Expr {
	sqls := make([]string, len(exprs))
	var vars []any
	for i, e := range exprs {
		sqls[i] = e.SQL
		vars = append(vars, e.Vars...)
	}
	return clause.Expr{SQL: "(" + strings.Join(sqls, op) + ")", Vars: vars}
}

// all: every word matches.
func (m matcher) all() clause.Expr {
	var out []clause.Expr
	for _, w := range m.words {
		out = append(out, m.word(w))
	}
	return join(out, " AND ")
}

// any: at least one word matches. Words under three letters match nearly
// everything on their own, so they only select songs when the query has
// nothing longer (they still count in the score).
func (m matcher) any() clause.Expr {
	var long []string
	fuzzy := false
	for _, w := range m.words {
		if utf8.RuneCountInString(w) >= 3 {
			long = append(long, w)
			fuzzy = fuzzy || m.canFuzz(w)
		}
	}
	if len(long) == 0 {
		return m.all()
	}
	if fuzzy {
		var branches []clause.Expr
		for _, w := range long {
			branches = append(branches, m.branches(w)...)
		}
		return union(branches)
	}
	var out []clause.Expr
	for _, w := range long {
		out = append(out, m.exact(w))
	}
	return join(out, " OR ")
}

// score ranks candidates, higher first.
func (m matcher) score() clause.Expr {
	var parts []clause.Expr
	for _, w := range m.words {
		e := m.exact(w)
		inside := "0.5"
		if utf8.RuneCountInString(w) < fuzzyMinRunes {
			inside = "0.25"
		}
		sql := "(CASE"
		vars := []any{}
		if m.indexed {
			// position of the hit: whole word, start of a word, inside one
			esc := likeEscape(w)
			sql += ` WHEN (' ' || search_text || ' ') LIKE ? ESCAPE '\' THEN 1.0 WHEN (' ' || search_text) LIKE ? ESCAPE '\' THEN 0.85`
			vars = append(vars, "% "+esc+" %", "% "+esc+"%")
		}
		sql += " WHEN " + e.SQL + " THEN " + inside
		vars = append(vars, e.Vars...)
		if m.canFuzz(w) {
			sql += " WHEN id IN (?) THEN 0.75 * word_similarity(?, search_text)"
			vars = append(vars, m.fuzzy[w], w)
		}
		parts = append(parts, clause.Expr{SQL: sql + " ELSE 0 END)", Vars: vars})
	}
	if len(m.words) > 1 {
		// the whole query, as a phrase or squashed ("dole yomwana" ->
		// "doleyomwana"), is worth more than its words apart
		phrase := m.exact(strings.Join(m.words, " "))
		squashed := m.exact(strings.Join(m.words, ""))
		parts = append(parts, clause.Expr{
			SQL:  "(CASE WHEN " + phrase.SQL + " OR " + squashed.SQL + " THEN 2.0 ELSE 0 END)",
			Vars: append(phrase.Vars, squashed.Vars...)})
	}
	return join(parts, " + ")
}

// containsAll reports whether every word is a substring of text.
func containsAll(text string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}
