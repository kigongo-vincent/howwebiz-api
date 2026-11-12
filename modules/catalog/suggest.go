package catalog

import "strings"

// Suggest respells a folded query after the song that answered it: each
// query word the song doesn't hold is replaced by the song's most similar
// title/artist word ("essense wizkid" + Essence/Wizkid -> "essence
// wizkid"). Empty when nothing changes or no word is similar enough.
func Suggest(folded string, top Song) string {
	vocab := strings.Fields(Fold(top.Title) + " " + Fold(top.Artist) + " " + Fold(strings.Join(top.Featured, " ")))
	text := SearchText(top.Title, top.Artist, top.Featured, top.Key)
	words := strings.Fields(folded)
	changed := false
	for i, w := range words {
		if strings.Contains(text, SearchFold(w)) {
			continue
		}
		best, bestSim := "", 0.0
		for _, v := range vocab {
			if sim := trigramSimilarity(w, v); sim > bestSim {
				best, bestSim = v, sim
			}
		}
		if bestSim >= 0.3 && best != w {
			words[i], changed = best, true
		}
	}
	if !changed {
		return ""
	}
	return strings.Join(words, " ")
}

// trigramSimilarity is pg_trgm's similarity: shared trigrams of the
// space-padded words over all distinct trigrams.
func trigramSimilarity(a, b string) float64 {
	ta, tb := trigrams(a), trigrams(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	shared := 0
	for g := range ta {
		if tb[g] {
			shared++
		}
	}
	return float64(shared) / float64(len(ta)+len(tb)-shared)
}

func trigrams(w string) map[string]bool {
	r := []rune("  " + w + " ")
	out := map[string]bool{}
	for i := 0; i+3 <= len(r); i++ {
		out[string(r[i:i+3])] = true
	}
	return out
}
