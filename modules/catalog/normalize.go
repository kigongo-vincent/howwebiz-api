package catalog

import (
	"regexp"
	"strings"
	"unicode"

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
	foldChain   = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
)

// Fold lowercases, removes accents and punctuation, and collapses spaces.
func Fold(s string) string {
	if out, _, err := transform.String(foldChain, s); err == nil {
		s = out
	}
	s = strings.ReplaceAll(strings.ToLower(s), "&", " and ")
	return strings.TrimSpace(nonAlnumRe.ReplaceAllString(s, " "))
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
		Key:      Fold(primary) + "|" + Fold(cleanTitle),
		Title:    cleanTitle,
		Artist:   primary,
		Featured: feat,
	}
}
