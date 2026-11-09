package scraper

import (
	"testing"
	"time"
)

// audio.com's robots.txt as served on 2026-11-09.
const audioComRobots = `User-Agent: *
Allow: /search
Allow: /*?page=
Allow: /search/category/*?page=
Allow: /auth/sign-in
Disallow: *?*
Disallow: /auth
Disallow: /audacity/link
Disallow: /audacity/upload
Disallow: /my-audio
Disallow: /audio/upload
Disallow: /profile
Disallow: /404
Disallow: /privacy
Disallow: /anonymous/*
Disallow: /*/*/edit
Sitemap: https://audio.com/sitemap.xml`

func TestRobotsAudioCom(t *testing.T) {
	r := ParseRobots(audioComRobots, "howwebiz-api-crawler/1.0")
	cases := map[string]bool{
		"https://audio.com/genres":                                        true,
		"https://audio.com/genres/pop":                                    true,
		"https://audio.com/genres/pop?page=2":                             true, // Allow /*?page= beats Disallow *?*
		"https://audio.com/search?search_text=bnxn&sort=relevance":        true, // Allow /search beats *?*
		"https://audio.com/search?search_text=bnxn&sort=relevance&page=3": true,
		"https://audio.com/genres/pop?sort=new":                           false, // other query strings are disallowed
		"https://audio.com/genres/pop?sort=new&page=2":                    false, // "?page=" must follow the path directly
		"https://audio.com/auth/login":                                    false,
		"https://audio.com/auth/sign-in":                                  true,
		"https://audio.com/someone/audio/track/edit":                      false,
		"https://audio.com/anonymous/x":                                   false,
	}
	for u, want := range cases {
		if got := r.Allowed(u); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", u, got, want)
		}
	}
}

func TestRobotsGroupsAndAnchors(t *testing.T) {
	body := "User-agent: badbot\nDisallow: /\n\nUser-agent: *\nDisallow: /private$\nDisallow: /tmp/\n"
	if ParseRobots(body, "BadBot/2.0").Allowed("https://x/anything") {
		t.Error("badbot should be fully disallowed")
	}
	r := ParseRobots(body, "goodbot")
	if !r.Allowed("https://x/private/sub") || r.Allowed("https://x/private") || r.Allowed("https://x/tmp/a") {
		t.Error("anchor / prefix rules wrong")
	}
	if !ParseRobots("", "x").Allowed("https://x/a?b=c") {
		t.Error("empty robots should allow")
	}
}

// ugamusic.net's robots.txt (2026-11-09): pagination and search are
// disallowed, plain song pages and sitemaps are not; Crawl-delay 1.
const ugamusicRobots = `User-agent: *
Allow: /
Disallow: /admin/
Disallow: /api/
Disallow: /storage/
Disallow: /*.json$
Disallow: /*?*sort=
Disallow: /*?*page=
Disallow: /search?
Crawl-delay: 1

User-agent: Googlebot
Allow: /
Crawl-delay: 0.5`

func TestRobotsUgamusicAndCrawlDelay(t *testing.T) {
	r := ParseRobots(ugamusicRobots, "howwebiz-api-crawler/1.0")
	for u, want := range map[string]bool{
		"https://ugamusic.net/songs":                                 true,
		"https://ugamusic.net/songs?page=2":                          false,
		"https://ugamusic.net/search?q=kenzo":                        false,
		"https://ugamusic.net/songs/nkutwaleko-by-biswanka-download": true,
		"https://ugamusic.net/sitemap/songs":                         true,
		"https://ugamusic.net/storage/song_url/x.mp3":                false,
	} {
		if got := r.Allowed(u); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", u, got, want)
		}
	}
	if r.CrawlDelay != time.Second {
		t.Errorf("crawl delay = %v, want 1s", r.CrawlDelay)
	}
	if g := ParseRobots(ugamusicRobots, "Googlebot/2.1"); g.CrawlDelay != 500*time.Millisecond {
		t.Errorf("googlebot crawl delay = %v", g.CrawlDelay)
	}
}
