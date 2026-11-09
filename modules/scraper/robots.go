package scraper

import (
	"bufio"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Robots is a robots.txt rule set for one user agent, matched the way
// Google documents it: the longest matching rule wins, Allow wins ties,
// `*` matches any run of characters and a trailing `$` anchors the end.
type Robots struct {
	rules []robotsRule
	// CrawlDelay is the group's Crawl-delay (0 if none).
	CrawlDelay time.Duration
}

type robotsRule struct {
	allow   bool
	pattern string
	re      *regexp.Regexp
}

// ParseRobots extracts the rules that apply to agent (falling back to the
// `*` group). Unknown directives are ignored.
func ParseRobots(body, agent string) *Robots {
	type group struct {
		agents []string
		rules  []robotsRule
		delay  time.Duration
	}
	var groups []*group
	var cur *group
	lastWasAgent := false
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(val)
		switch key {
		case "user-agent":
			if !lastWasAgent || cur == nil {
				cur = &group{}
				groups = append(groups, cur)
			}
			cur.agents = append(cur.agents, strings.ToLower(val))
			lastWasAgent = true
		case "allow", "disallow":
			lastWasAgent = false
			if cur == nil || val == "" {
				continue // empty Disallow = allow everything
			}
			cur.rules = append(cur.rules, robotsRule{allow: key == "allow", pattern: val, re: robotsPattern(val)})
		case "crawl-delay":
			lastWasAgent = false
			if cur != nil {
				if f, err := strconv.ParseFloat(val, 64); err == nil && f > 0 {
					cur.delay = time.Duration(f * float64(time.Second))
				}
			}
		default:
			lastWasAgent = false
		}
	}
	agent = strings.ToLower(agent)
	var star *group
	for _, g := range groups {
		for _, a := range g.agents {
			if a != "*" && a != "" && strings.Contains(agent, a) {
				return &Robots{rules: g.rules, CrawlDelay: g.delay}
			}
			if a == "*" {
				star = g
			}
		}
	}
	if star == nil {
		return &Robots{}
	}
	return &Robots{rules: star.rules, CrawlDelay: star.delay}
}

func robotsPattern(p string) *regexp.Regexp {
	anchored := strings.HasSuffix(p, "$")
	p = strings.TrimSuffix(p, "$")
	parts := strings.Split(p, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	expr := "^" + strings.Join(parts, ".*")
	if anchored {
		expr += "$"
	}
	return regexp.MustCompile(expr)
}

// Allowed reports whether the path+query of rawURL may be fetched.
func (r *Robots) Allowed(rawURL string) bool {
	if r == nil {
		return true
	}
	target := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		target = u.EscapedPath()
		if target == "" {
			target = "/"
		}
		if u.RawQuery != "" {
			target += "?" + u.RawQuery
		}
	}
	best, allowed := -1, true
	for _, rule := range r.rules {
		if !rule.re.MatchString(target) {
			continue
		}
		n := len(rule.pattern)
		if n > best || (n == best && rule.allow) {
			best, allowed = n, rule.allow
		}
	}
	return allowed
}
