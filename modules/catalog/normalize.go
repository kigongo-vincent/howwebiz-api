package catalog

import (
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Matching: two providers' songs are the same song when their keys are
// equal. A key is "<primary artist>|<title>", both folded (lowercase, no
// accents or punctuation) and stripped of packaging noise such as
// "(Official Audio)" or "ft. X". Version words that make a different
// recording (remix, live, acoustic, ...) are kept.

var (
	bracketRe = regexp.MustCompile(`\s*[\(\[\{]([^\)\]\}]*)[\)\]\}]`)
	// Bracket contents that are packaging, not part of the song's identity.
	noiseRe = regexp.MustCompile(`(?i)^\s*(official\s*)?(music\s*)?(audio|video|lyrics?|lyric\s*video|visuali[sz]er|mp3|hd|hq|4k|explicit|clean|out\s*now|new|new\s*song|\d{3,4}\s*k|mp3_?\d+k|prod\.?.*|produced\s+by.*|dir\.?.*|directed\s+by.*|official)(\s*(audio|video|lyrics?))?\s*$`)
	featRe  = regexp.MustCompile(`(?i)\s*[\(\[]?\b(ft|feat|featuring)\b\.?\s+([^\)\]]+)[\)\]]?`)
	// Separators between artist names: "A ft. B", "A x B", "A & B", "A, B".
	artistSepRe = regexp.MustCompile(`(?i)\s*(?:\bft\b\.?|\bfeat\b\.?|\bfeaturing\b|\bx\b|&|,|\band\b|\bvs\.?\b|\bwith\b|/|\+)\s*`)
	nonAlnumRe  = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	// A transform.Chain is stateful and NOT safe for concurrent use: one
	// shared chain crashed the process ("slice bounds out of range") once
	// several provider crawls ingested at the same time. One per goroutine.
	foldChains = sync.Pool{New: func() any {
		return transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	}}
)

// Fold lowercases, removes accents and punctuation, and collapses spaces.
// Safe for concurrent use.
func Fold(s string) string {
	t := foldChains.Get().(transform.Transformer)
	if out, _, err := transform.String(t, s); err == nil { // String resets t first
		s = out
	}
	foldChains.Put(t)
	s = strings.ReplaceAll(strings.ToLower(s), "&", " and ")
	return strings.TrimSpace(nonAlnumRe.ReplaceAllString(s, " "))
}

const (
	KindSong = "song"
	KindMix  = "mix"
)

// MixMinMs: a "song" this long is a DJ mix, nonstop or show (the longest
// kadongo kamu epics run 10-15 minutes; DJ sets start around 20).
const MixMinMs = 20 * 60 * 1000

// showMinMs: a track titled like a show ("radio show", "episode") is one
// from this length; a 3-minute song called "The Show" stays a song.
const showMinMs = 10 * 60 * 1000

var (
	// "Gospel Nonstop", "Kadongo Kamu Mixtape", "Megamix", "DJ Mix",
	// "Ugandan Mixes 2024"; never "Remix"
	mixWordRe = regexp.MustCompile(`(?i)\b(non[\s-]?stops?|mix[\s-]?tapes?|mega[\s-]?mix(es)?|dj[\s-]?mix(es)?|mixes)\b`)
	// "Mix Vol 3", "Mix 2024", "Mix 12"; a song called "Love Mix" stays a song
	mixVolRe = regexp.MustCompile(`(?i)\bmix\b\s*(vol(ume)?\.?\s*\d+|#?\d+)`)
	showRe   = regexp.MustCompile(`(?i)\b(show|episode|ep\.?\s*\d+|podcast|radio|broadcast|live\s+set|dj\s+set)\b`)
)

// IsMix reports whether a track is a DJ mix / nonstop / mixtape rather
// than a song: by its title, or a length no song has.
func IsMix(title string, durationMs int) bool {
	return durationMs >= MixMinMs || mixWordRe.MatchString(title) || mixVolRe.MatchString(title) ||
		(durationMs >= showMinMs && showRe.MatchString(title))
}

// KindOf is the track's kind: the provider's word, else IsMix.
func KindOf(t Track) string {
	switch {
	case t.Kind != "":
		return t.Kind
	case IsMix(t.Title, t.DurationMs):
		return KindMix
	}
	return KindSong
}

// SearchTextVersion changes whenever SearchText does; rows stored with an
// older version are rewritten in the background (PrepareSearchText).
const SearchTextVersion = 4 // 3: reclassifies kind (mixes); 4: doubled letters, title first

// SearchText folds everything a text query may match — title, artist,
// featured artists and the match key — into one string. Queries are folded
// the same way, so accents and punctuation never prevent a hit.
//
// Each multi-word field is also stored squashed: "Dole Y'omwana" folds to
// "dole y omwana" and adds "doleyomwana", so a user typing "yomwana",
// "y omwana" or "doleyomwana" finds it.
//
// Doubled letters are collapsed (SearchFold): Luganda spellings vary by them
// ("omwaana"/"omwana", "Ssebatta"/"Sebata"), and queries collapse the same.
//
// The text starts with the squashed title, so "the title is exactly the
// query" is a prefix match (TitleIs).
func SearchText(title, artist string, featured []string, key string) string {
	fields := append([]string{SearchFold(title), SearchFold(artist)}, SearchFold(strings.ReplaceAll(key, "|", " ")))
	for _, f := range featured {
		fields = append(fields, SearchFold(f))
	}
	parts := []string{strings.ReplaceAll(fields[0], " ", "")}
	parts = append(parts, fields...)
	for _, f := range fields[1:] {
		if strings.Contains(f, " ") {
			parts = append(parts, strings.ReplaceAll(f, " ", ""))
		}
	}
	return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), " ")
}

// SearchFold is Fold with doubled letters collapsed: what search_text holds
// and queries are matched as.
func SearchFold(s string) string {
	var b strings.Builder
	var prev rune
	for _, r := range Fold(s) {
		if r != prev || r == ' ' || unicode.IsDigit(r) { // "2000" stays
			b.WriteRune(r)
		}
		prev = r
	}
	return b.String()
}

// CleanTitle removes packaging noise and featured-artist credits from a
// title and returns the featured artists it found.
func CleanTitle(title string) (clean string, featured []string) {
	t := bracketRe.ReplaceAllStringFunc(title, func(m string) string {
		inner := strings.TrimSpace(bracketRe.FindStringSubmatch(m)[1])
		if noiseRe.MatchString(inner) {
			return ""
		}
		if f := featRe.FindStringSubmatch("(" + inner + ")"); f != nil && strings.HasPrefix(strings.ToLower(strings.TrimLeft(inner, " ")), strings.ToLower(f[1])) {
			featured = append(featured, splitArtists(f[2])...)
			return ""
		}
		return m
	})
	// Trailing, unbracketed "ft. X".
	if f := featRe.FindStringSubmatchIndex(t); f != nil {
		featured = append(featured, splitArtists(t[f[4]:f[5]])...)
		t = t[:f[0]]
	}
	// File-name style "Artist - Title": keep the title part only when the
	// caller passes it separately; here just tidy separators/whitespace.
	return strings.Join(strings.Fields(t), " "), featured
}

func splitArtists(s string) []string {
	var out []string
	for _, p := range artistSepRe.Split(s, -1) {
		if p = strings.TrimSpace(strings.Trim(p, "()[] ")); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SplitArtist returns the primary artist and the others credited.
func SplitArtist(artist string) (primary string, others []string) {
	parts := splitArtists(artist)
	if len(parts) == 0 {
		return strings.TrimSpace(artist), nil
	}
	return parts[0], parts[1:]
}

// Identity is a track's normalized identity.
type Identity struct {
	Key      string   // matching key
	Title    string   // title without noise/credits (display)
	Artist   string   // primary artist (display)
	Featured []string // other credited artists (display, deduplicated)
}

// Column sizes in models.go: values are clipped to fit, never rejected.
const (
	keySize        = 400
	genreSize      = 64
	externalIDSize = 128
)

// Clip shortens s to at most n characters (runes), never splitting one.
func Clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// CleanGenre makes a provider's genre fit the catalog: free-text lists
// ("Rock, Stoner Rock, Doom Metal") keep their first entry.
func CleanGenre(g string) string {
	if first, _, ok := strings.Cut(g, ","); ok {
		g = first
	}
	return Clip(strings.TrimSpace(g), genreSize)
}

// Identify computes the identity of a title/artist pair.
func Identify(title, artist string, featured ...string) Identity {
	cleanTitle, ft := CleanTitle(title)
	primary, others := SplitArtist(artist)
	// "Artist - Title" titles (file-name style) when the artist repeats.
	if a, rest, ok := strings.Cut(cleanTitle, " - "); ok && Fold(a) == Fold(primary) {
		cleanTitle = strings.TrimSpace(rest)
	}
	seen := map[string]bool{Fold(primary): true}
	var feat []string
	for _, f := range append(append(others, ft...), featured...) {
		if k := Fold(f); k != "" && !seen[k] {
			seen[k] = true
			feat = append(feat, strings.TrimSpace(f))
		}
	}
	return Identity{
		Key:      Clip(Fold(primary)+"|"+Fold(cleanTitle), keySize),
		Title:    cleanTitle,
		Artist:   primary,
		Featured: feat,
	}
}
