package ncs

import (
	"os"
	"strings"
	"testing"
)

func TestParseList(t *testing.T) {
	body, err := os.ReadFile("testdata/home_page1.html")
	if err != nil {
		t.Fatal(err)
	}
	site := Site{Base: "https://ncs.io"}
	tracks, err := site.ParseList(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) < 10 {
		t.Fatalf("got %d tracks", len(tracks))
	}
	seen := map[string]bool{}
	for _, tr := range tracks {
		if seen[tr.ExternalID] {
			t.Errorf("duplicate %s", tr.ExternalID)
		}
		seen[tr.ExternalID] = true
		if tr.Title == "" || tr.Artist == "" || !strings.HasPrefix(tr.AudioURL, "https://ncsmusic.s3.") || !strings.HasSuffix(tr.AudioURL, ".mp3") {
			t.Errorf("incomplete: %+v", tr)
		}
	}
	if site.ListPath(3) != "/?display=&page=3" {
		t.Error("ListPath")
	}
	if empty, _ := site.ParseList([]byte("<html><body>no releases</body></html>")); len(empty) != 0 {
		t.Error("past the end should be empty")
	}
}
