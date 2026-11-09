package audiocom

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// RSCPage builds a synthetic Next.js App Router page whose flight data
// (self.__next_f.push) carries the given objects, split across two chunks
// the way Next streams them.
func rscPage(title string, objects []map[string]any, links ...string) string {
	payload, _ := json.Marshal([]any{"$", "div", nil, map[string]any{"children": map[string]any{"items": objects}}})
	flight := "0:[\"$\",\"$L1\",null,{}]\n5:" + string(payload) + "\n"
	half := len(flight) / 2
	var b strings.Builder
	fmt.Fprintf(&b, "<!DOCTYPE html><html><head><title>%s</title></head><body>", title)
	for _, l := range links {
		fmt.Fprintf(&b, `<a href="%s">x</a>`, l)
	}
	for _, chunk := range []string{flight[:half], flight[half:]} {
		q, _ := json.Marshal(chunk)
		fmt.Fprintf(&b, "<script>self.__next_f.push([1,%s])</script>", q)
	}
	b.WriteString("</body></html>")
	return b.String()
}

func TestExtractRSC(t *testing.T) {
	page := rscPage("Pop", []map[string]any{
		{"id": "a1", "title": "One", "slug": "one", "username": "u", "author_name": "U",
			"tags": []any{"pop"}, "play": map[string]any{"url": "https://s3.audio.com/a1.mp3", "duration": 1000}},
		// camelCase variant with nested author and tag objects
		{"id": "a2", "title": "Two", "slug": "two", "author": map[string]any{"username": "v", "name": "V"},
			"playUrl": "https://s3.audio.com/a2.mp3", "durationMs": 2000, "tags": []any{map[string]any{"name": "rock"}}},
		// not audio: a collection and an author
		{"id": "c1", "title": "Album", "items_count": 3, "slug": "album", "username": "u"},
		{"id": "u1", "username": "u", "name": "U"},
	}, "/genres/pop", "https://audio.com/genres/hip-hop", "/genres/pop?page=2", "/u/audio/one")

	pd := ExtractPage([]byte(page))
	if len(pd.Tracks) != 2 {
		t.Fatalf("want 2 tracks, got %d: %+v", len(pd.Tracks), pd.Tracks)
	}
	a, b := pd.Tracks[0], pd.Tracks[1]
	if a.ID != "a1" || a.Play.URL != "https://s3.audio.com/a1.mp3" || a.Play.Duration != 1000 || a.AuthorName != "U" || a.Tags[0] != "pop" {
		t.Errorf("a1: %+v", a)
	}
	if b.Username != "v" || b.AuthorName != "V" || b.Play.URL != "https://s3.audio.com/a2.mp3" || b.Play.Duration != 2000 || b.Tags[0] != "rock" {
		t.Errorf("a2: %+v", b)
	}
	if strings.Join(pd.Genres, ",") != "pop,hip-hop" || pd.Title != "Pop" || pd.Sources[0] != "rsc" {
		t.Errorf("genres=%v title=%q sources=%v", pd.Genres, pd.Title, pd.Sources)
	}
}

func TestExtractNextDataAndJSONLD(t *testing.T) {
	page := `<html><head><script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"audios":[{"id":"n1","title":"Next","slug":"next","username":"z","play":{"url":"https://s3/n1.mp3"}}]}}}` +
		`</script><script type="application/ld+json">` +
		`{"@context":"https://schema.org","@type":"ItemList","itemListElement":[{"@type":"ListItem","item":` +
		`{"@type":"MusicRecording","name":"Song LD","url":"https://audio.com/artist/audio/song-ld","duration":"PT3M20S",` +
		`"byArtist":{"name":"Artist"},"audio":{"@type":"AudioObject","contentUrl":"https://s3/ld.mp3"},"keywords":"afro, pop"}}]}` +
		`</script></head><body></body></html>`
	pd := ExtractPage([]byte(page))
	if len(pd.Tracks) != 2 {
		t.Fatalf("want 2 tracks, got %+v", pd.Tracks)
	}
	ld := pd.Tracks[1]
	if ld.ID != "url:artist/audio/song-ld" || ld.Username != "artist" || ld.Slug != "song-ld" || ld.AuthorName != "Artist" ||
		ld.Play.URL != "https://s3/ld.mp3" || ld.Play.Duration != 200000 || len(ld.Tags) != 2 {
		t.Errorf("json-ld: %+v", ld)
	}
}

func TestExtractNothing(t *testing.T) {
	pd := ExtractPage([]byte(`<html><head><title>Just a moment...</title></head><body>hi</body></html>`))
	if len(pd.Tracks) != 0 || len(pd.Genres) != 0 || pd.Title != "Just a moment..." {
		t.Fatalf("unexpected: %+v", pd)
	}
}
