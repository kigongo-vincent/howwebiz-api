package catalog

import (
	"reflect"
	"strings"
	"testing"
)

func TestIdentifyMatchesAcrossProviders(t *testing.T) {
	// The same recording as different providers present it (real examples).
	same := [][2]string{
		{"Nassanga (Remix)", "Kalifah AgaNaga ft. Eddy Kenzo, Winnie Nwagi, Lil Pazo, Weasel"}, // howwe
		{"Nassanga (Remix) (Official Audio)", "Kalifah AgaNaga"},                               // file-name style
		{"Nassanga (Remix) ft. Eddy Kenzo", "Kalifah AgaNaga"},
		{"NASSANGA (REMIX)", "kalifah aganaga & Eddy Kenzo"},
		{"Kalifah AgaNaga - Nassanga (Remix)", "Kalifah Aganaga"}, // "Artist - Title"
	}
	want := Identify(same[0][0], same[0][1]).Key
	if want != "kalifah aganaga|nassanga remix" {
		t.Fatalf("key = %q", want)
	}
	for _, s := range same[1:] {
		if got := Identify(s[0], s[1]).Key; got != want {
			t.Errorf("Identify(%q, %q) = %q, want %q", s[0], s[1], got, want)
		}
	}
}

func TestIdentifyKeepsDifferentVersionsApart(t *testing.T) {
	a := Identify("Nassanga", "Kalifah AgaNaga").Key
	b := Identify("Nassanga (Remix)", "Kalifah AgaNaga").Key
	c := Identify("Nassanga (Live)", "Kalifah AgaNaga").Key
	d := Identify("Nassanga", "Spice Diana").Key
	if a == b || a == c || b == c || a == d {
		t.Fatalf("versions/artists collapsed: %q %q %q %q", a, b, c, d)
	}
}

func TestIdentifyNoiseAndAccents(t *testing.T) {
	cases := []struct{ title, artist, key string }{
		{"Yeffe (Official Audio)(MP3_160K)", "Martha Mukisa", "martha mukisa|yeffe"},
		{"Embilizi ( Audio )", "Echoled", "echoled|embilizi"},
		{"Sitya Loss [Official Video] (Prod. by Washington)", "Eddy Kenzo", "eddy kenzo|sitya loss"},
		{"Café Nkwagala", "Bébé Cool", "bebe cool|cafe nkwagala"},
		{"Gundi (Remix)", "Kapeke", "kapeke|gundi remix"},
		{"Twingana 100%", "M-rex And Robstar", "m rex|twingana 100"},
		{"Outside", "SUCCIDO, Karole Kasita", "succido|outside"},
	}
	for _, c := range cases {
		if got := Identify(c.title, c.artist).Key; got != c.key {
			t.Errorf("Identify(%q, %q) = %q, want %q", c.title, c.artist, got, c.key)
		}
	}
}

func TestIdentifyDisplayParts(t *testing.T) {
	id := Identify("Nassanga (Remix) (Official Audio)", "Kalifah AgaNaga ft. Eddy Kenzo, Winnie Nwagi", "Eddy Kenzo", "Weasel")
	if id.Title != "Nassanga (Remix)" || id.Artist != "Kalifah AgaNaga" ||
		!reflect.DeepEqual(id.Featured, []string{"Eddy Kenzo", "Winnie Nwagi", "Weasel"}) {
		t.Fatalf("%+v", id)
	}
}

// Postgres rejects values longer than a varchar column, failing the whole
// batch; free-text provider values are cleaned to fit instead.
func TestCleanGenreAndClip(t *testing.T) {
	for in, want := range map[string]string{
		"Rock, Stoner Rock, Doom Metal, Heavy Metal, Instrumental, Hard Rock, Fuzz": "Rock",
		" Afrobeats ":           "Afrobeats",
		"":                      "",
		strings.Repeat("é", 80): strings.Repeat("é", 64),
	} {
		if got := CleanGenre(in); got != want {
			t.Errorf("CleanGenre(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Identify(strings.Repeat("long title ", 60), "Artist").Key; len([]rune(got)) > 400 {
		t.Errorf("key has %d runes, column holds 400", len([]rune(got)))
	}
}

func TestIsMix(t *testing.T) {
	mixes := []string{"Gospel Nonstop Vol 3", "Super Classic Kadongo Kamu 80s, 90s & 00s MiXtape", "Ugandan Mixes 2024",
		"Afrobeat Megamix", "DJ Mix - Kampala Nights", "Lugaba Non-Stop", "Club Bangers Mix Vol. 2", "Throwback Mix 2019", "Mix #12"}
	songs := []string{"Nassanga (Remix)", "Love Mix", "Mixed Feelings", "Remixes", "Sitya Loss", "Dole Y'omwana", "Mixtures"}
	for _, m := range mixes {
		if !IsMix(m, 4*60*1000) {
			t.Errorf("%q should be a mix", m)
		}
	}
	for _, s := range songs {
		if IsMix(s, 4*60*1000) {
			t.Errorf("%q should be a song", s)
		}
	}
	if !IsMix("Untitled", 40*60*1000) || IsMix("Walumbe Zaaya", 14*60*1000) {
		t.Error("length: 40 min is a mix, a 14 min kadongo kamu epic is not")
	}
}
