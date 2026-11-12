package catalog

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm/clause"
)

// Completion is one search-as-you-type suggestion.
type Completion struct {
	ID       uint   `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	CoverURL string `json:"coverUrl"`
}

// completeTTL: suggestions for a prefix change slowly.
const completeTTL = 5 * time.Minute

// Complete suggests songs while the user types: every word but the last
// must be in the song, and the last may be the start of a word ("calm d"
// -> "Calm Down"). Titles starting with the input come first, then
// popular songs. Songs only (no mixes); at most limit (1-20).
func (s *SearchService) Complete(ctx context.Context, input string, limit int) ([]Completion, error) {
	words := strings.Fields(SearchFold(input))
	limit = min(max(limit, 1), 20)
	out := []Completion{}
	if len(words) == 0 || utf8.RuneCountInString(strings.Join(words, "")) < 2 {
		return out, nil
	}
	key := "complete:" + s.Cache.Generation(ctx, cacheNS) + ":" + cacheKey(words, limit)
	if s.Cache.Get(ctx, key, &out) {
		return out, nil
	}
	db := s.DB.WithContext(ctx).Model(&Song{}).Where("kind = ?", KindSong)
	// every word but the last held anywhere, the last starting a word...
	last := words[len(words)-1]
	var conds []string
	var args []any
	for _, w := range words[:len(words)-1] {
		conds = append(conds, `search_text LIKE ? ESCAPE '\'`)
		args = append(args, "%"+likeEscape(w)+"%")
	}
	conds = append(conds, `(search_text LIKE ? ESCAPE '\' OR search_text LIKE ? ESCAPE '\')`)
	args = append(args, likeEscape(last)+"%", "% "+likeEscape(last)+"%")
	// ...or the whole input, squashed, starting a word: "dole yomw" is the
	// start of "doleyomwana" (Dole Y'omwaana), not of any one word
	squashed := likeEscape(strings.Join(words, ""))
	db = db.Where("(("+strings.Join(conds, " AND ")+`) OR search_text LIKE ? ESCAPE '\' OR search_text LIKE ? ESCAPE '\')`,
		append(args, squashed+"%", "% "+squashed+"%")...)
	db = db.Order(clause.OrderBy{Expression: clause.Expr{
		// search_text starts with the squashed title: titles that start
		// with the input first
		SQL:                `CASE WHEN search_text LIKE ? ESCAPE '\' THEN 0 ELSE 1 END, ` + popularity + ` + ` + spread + ` DESC, id DESC`,
		Vars:               []any{likeEscape(strings.Join(words, "")) + "%"},
		WithoutParentheses: true}})
	var songs []Song
	if err := db.Select("id", "title", "artist", "cover_url").Limit(limit).Find(&songs).Error; err != nil {
		return nil, err
	}
	for _, x := range songs {
		out = append(out, Completion{ID: x.ID, Title: x.Title, Artist: x.Artist, CoverURL: x.CoverURL})
	}
	s.Cache.Set(ctx, key, out, completeTTL)
	return out, nil
}
