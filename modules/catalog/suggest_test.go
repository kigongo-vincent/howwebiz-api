package catalog

import "testing"

func TestSuggest(t *testing.T) {
	top := Song{Title: "Essence", Artist: "Wizkid", Featured: []string{"Tems"}, Key: "wizkid|essence"}
	cases := map[string]string{
		"essense wizkid": "essence wizkid",
		"essense":        "essence",
		"essence wizkid": "", // nothing to correct
		"zzzz":           "", // nothing similar enough
		"wizkit tems":    "wizkid tems",
	}
	for q, want := range cases {
		if got := Suggest(Fold(q), top); got != want {
			t.Errorf("Suggest(%q) = %q, want %q", q, got, want)
		}
	}
	if s := trigramSimilarity("nkwagla", "nkwagala"); s < 0.5 || s > 0.7 {
		t.Errorf("similarity %v", s)
	}
}
