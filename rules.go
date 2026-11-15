package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// RuleConfig is the on-disk shape of a single detection rule.
type RuleConfig struct {
	Name string `yaml:"name"`
	// Phrases are literal strings matched case-insensitively as substrings.
	Phrases []string `yaml:"phrases"`
	// Patterns are Go RE2 regular expressions. Use (?i) for case-insensitivity.
	Patterns []string `yaml:"patterns"`
	// ReportReason is the Bluesky report reason used by the Discord report
	// button; any key of reportReasons in bsky.go. Default spam.
	ReportReason string `yaml:"report_reason"`
	// AutoReport files a Bluesky report automatically, with no human in the
	// loop, whenever this rule fires. Requires BSKY_HANDLE/BSKY_APP_PASSWORD.
	AutoReport bool `yaml:"auto_report"`
	// Collections restricts the rule to these record NSIDs (keys of
	// extractors in extract.go). Empty means every collection.
	Collections []string `yaml:"collections"`
	// Langs restricts the rule to records that declare one of these BCP-47
	// language tags. "pt" also matches "pt-BR". Empty means any language.
	// Records that declare no language (all profiles, some posts) never match
	// a rule with langs set.
	Langs []string `yaml:"langs"`
}

type RulesFile struct {
	Rules []RuleConfig `yaml:"rules"`
}

type compiledRule struct {
	name     string
	reason   string
	auto     bool
	phrases  []string // lowercased
	patterns []*regexp.Regexp
	// collections is nil for an unscoped rule.
	collections map[string]bool
	// langs is nil for a rule that runs on every language. Lowercased.
	langs []string
}

// Matcher evaluates text against a set of compiled rules. It is safe for
// concurrent use.
type Matcher struct {
	rules []compiledRule
	auto  bool // any rule has auto_report
}

// RuleHit is one rule that matched, with the specific terms that triggered it.
type RuleHit struct {
	Rule   string `json:"rule"`
	Reason string `json:"report_reason"`
	// AutoReport is true when the rule is configured to file a report
	// without human confirmation.
	AutoReport bool     `json:"auto_report,omitempty"`
	Phrases    []string `json:"phrases,omitempty"`
	Matches    []string `json:"matches,omitempty"` // text matched by regex patterns
}

func LoadRules(path string) (*Matcher, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rf RulesFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return NewMatcher(rf.Rules)
}

func NewMatcher(cfgs []RuleConfig) (*Matcher, error) {
	m := &Matcher{}
	seen := map[string]bool{}
	for i, rc := range cfgs {
		if rc.Name == "" {
			return nil, fmt.Errorf("rule %d: missing name", i)
		}
		if seen[rc.Name] {
			return nil, fmt.Errorf("rule %q: duplicate name", rc.Name)
		}
		seen[rc.Name] = true
		if len(rc.Phrases) == 0 && len(rc.Patterns) == 0 {
			return nil, fmt.Errorf("rule %q: needs at least one phrase or pattern", rc.Name)
		}
		cr := compiledRule{name: rc.Name, reason: rc.ReportReason, auto: rc.AutoReport}
		m.auto = m.auto || rc.AutoReport
		if cr.reason == "" {
			cr.reason = "spam"
		}
		if _, ok := reportReasons[cr.reason]; !ok {
			return nil, fmt.Errorf("rule %q: unknown report_reason %q", rc.Name, cr.reason)
		}
		for _, c := range rc.Collections {
			if _, ok := extractors[c]; !ok {
				return nil, fmt.Errorf("rule %q: unknown collection %q", rc.Name, c)
			}
			if cr.collections == nil {
				cr.collections = map[string]bool{}
			}
			cr.collections[c] = true
		}
		for _, l := range rc.Langs {
			l = strings.ToLower(strings.TrimSpace(l))
			if !langTag.MatchString(l) {
				return nil, fmt.Errorf("rule %q: invalid language tag %q", rc.Name, l)
			}
			cr.langs = append(cr.langs, l)
		}
		for _, p := range rc.Phrases {
			if p = strings.TrimSpace(p); p != "" {
				cr.phrases = append(cr.phrases, strings.ToLower(p))
			}
		}
		for _, p := range rc.Patterns {
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("rule %q: pattern %q: %w", rc.Name, p, err)
			}
			cr.patterns = append(cr.patterns, re)
		}
		m.rules = append(m.rules, cr)
	}
	return m, nil
}

// langTag is a loose BCP-47 check: alphanumeric subtags joined by hyphens.
var langTag = regexp.MustCompile(`^[a-z]{1,8}(-[a-z0-9]{1,8})*$`)

// langMatch reports whether any declared tag is one of want or a subtag of
// one ("pt-br" is a "pt"). want is lowercased.
func langMatch(want, declared []string) bool {
	for _, d := range declared {
		d = strings.ToLower(d)
		for _, w := range want {
			if d == w || strings.HasPrefix(d, w+"-") {
				return true
			}
		}
	}
	return false
}

func (m *Matcher) Len() int { return len(m.rules) }

// HasAutoReport reports whether any rule is configured with auto_report.
func (m *Matcher) HasAutoReport() bool { return m.auto }

// Match returns every rule that fires on text from a record in collection
// that declares langs, or nil if none do. Rules scoped to other collections
// or languages are skipped.
func (m *Matcher) Match(collection string, langs []string, text string) []RuleHit {
	if text == "" {
		return nil
	}
	lower := strings.ToLower(text)
	var hits []RuleHit
	for _, r := range m.rules {
		if r.collections != nil && !r.collections[collection] {
			continue
		}
		if r.langs != nil && !langMatch(r.langs, langs) {
			continue
		}
		var h RuleHit
		for _, p := range r.phrases {
			if strings.Contains(lower, p) {
				h.Phrases = append(h.Phrases, p)
			}
		}
		for _, re := range r.patterns {
			if s := re.FindString(text); s != "" {
				h.Matches = append(h.Matches, s)
			}
		}
		if len(h.Phrases) > 0 || len(h.Matches) > 0 {
			h.Rule = r.name
			h.Reason = r.reason
			h.AutoReport = r.auto
			hits = append(hits, h)
		}
	}
	return hits
}
