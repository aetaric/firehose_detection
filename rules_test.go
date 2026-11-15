package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMatcher(t *testing.T) {
	m, err := NewMatcher([]RuleConfig{
		{Name: "phrase", Phrases: []string{"Free Money"}},
		{Name: "regex", Patterns: []string{`\b0x[a-fA-F0-9]{40}\b`}},
	})
	if err != nil {
		t.Fatal(err)
	}

	hits := m.Match("did:plc:test", "app.bsky.feed.post", nil, "get FREE MONEY now at 0x52908400098527886E0F7030069857D2E4169EE7")
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %+v", hits)
	}
	if hits[0].Rule != "phrase" || hits[0].Phrases[0] != "free money" {
		t.Errorf("phrase hit wrong: %+v", hits[0])
	}
	if hits[1].Rule != "regex" || len(hits[1].Matches) != 1 {
		t.Errorf("regex hit wrong: %+v", hits[1])
	}

	if hits := m.Match("did:plc:test", "app.bsky.feed.post", nil, "nothing to see here"); hits != nil {
		t.Errorf("want no hits, got %+v", hits)
	}
}

func TestNewMatcherRejectsBadRules(t *testing.T) {
	cases := map[string][]RuleConfig{
		"no name":   {{Phrases: []string{"x"}}},
		"empty":     {{Name: "a"}},
		"bad regex": {{Name: "a", Patterns: []string{"("}}},
		"duplicate": {{Name: "a", Phrases: []string{"x"}}, {Name: "a", Phrases: []string{"y"}}},
	}
	for name, cfg := range cases {
		if _, err := NewMatcher(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestPostText(t *testing.T) {
	rec := map[string]any{
		"text": "hello",
		"facets": []any{map[string]any{"features": []any{
			map[string]any{"$type": "app.bsky.richtext.facet#link", "uri": "https://bit.ly/abc"},
		}}},
		"embed": map[string]any{
			"$type":  "app.bsky.embed.recordWithMedia",
			"record": map[string]any{},
			"media": map[string]any{
				"images": []any{map[string]any{"alt": "a cat"}},
			},
		},
	}
	want := "hello\nhttps://bit.ly/abc\na cat"
	if got := postText(rec); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestProfileText(t *testing.T) {
	rec := map[string]any{
		"$type":       "app.bsky.actor.profile",
		"displayName": "Crypto King",
		"description": "DM for free eth",
		"website":     "https://example.com",
		"avatar":      map[string]any{"$type": "blob"},
	}
	want := "Crypto King\nDM for free eth\nhttps://example.com"
	if got := profileText(rec); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestLoadExampleRules(t *testing.T) {
	m, err := LoadRules("rules.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"FREE ETH for the first 100!":                         "crypto-giveaway",
		"we can recover stolen funds fast":                    "recovery-scam",
		"tip jar: 0x52908400098527886E0F7030069857D2E4169EE7": "wallet-address",
		"btc: bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq":     "wallet-address",
		"Bluesky Safety Team":                                 "staff-impersonation",
		"Buy 10k real followers today":                        "dm-solicitation",
		"check this https://bit.ly/abc123":                    "suspicious-shortlink",
		"order #1234567890123456789012345678 shipped":         "",
		"crossposted from https://t.co/abc123":                "",
		"just a normal post about my cat":                     "",
	}
	for text, want := range cases {
		hits := m.Match("did:plc:test", "app.bsky.feed.post", nil, text)
		got := ""
		if len(hits) > 0 {
			got = hits[0].Rule
		}
		if got != want {
			t.Errorf("%q: got rule %q, want %q", text, got, want)
		}
	}
}

func TestReportReasonDefaultsAndValidation(t *testing.T) {
	m, err := NewMatcher([]RuleConfig{{Name: "a", Phrases: []string{"x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if h := m.Match("did:plc:test", "app.bsky.feed.post", nil, "x"); h[0].Reason != "spam" {
		t.Errorf("default reason = %q", h[0].Reason)
	}
	if _, err := NewMatcher([]RuleConfig{{Name: "a", Phrases: []string{"x"}, ReportReason: "nope"}}); err == nil {
		t.Error("expected unknown report_reason to be rejected")
	}
}

func TestAutoReportPropagatesToHits(t *testing.T) {
	m, err := NewMatcher([]RuleConfig{
		{Name: "manual", Phrases: []string{"x"}},
		{Name: "auto", Phrases: []string{"y"}, AutoReport: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	hits := m.Match("did:plc:test", "app.bsky.feed.post", nil, "x y")
	if len(hits) != 2 || hits[0].AutoReport || !hits[1].AutoReport {
		t.Fatalf("auto_report not carried into hits: %+v", hits)
	}
	if !m.HasAutoReport() {
		t.Error("HasAutoReport should be true")
	}
	m2, _ := NewMatcher([]RuleConfig{{Name: "manual", Phrases: []string{"x"}}})
	if m2.HasAutoReport() {
		t.Error("HasAutoReport should be false with no auto_report rules")
	}
}

func TestAutoReportParsesFromYAML(t *testing.T) {
	var rf RulesFile
	if err := yaml.Unmarshal([]byte("rules:\n  - name: a\n    auto_report: true\n    phrases: [x]\n"), &rf); err != nil {
		t.Fatal(err)
	}
	if len(rf.Rules) != 1 || !rf.Rules[0].AutoReport {
		t.Fatalf("auto_report not parsed: %+v", rf.Rules)
	}
}

func TestMatcherCollectionScope(t *testing.T) {
	m, err := NewMatcher([]RuleConfig{
		{Name: "everywhere", Phrases: []string{"foo"}},
		{Name: "profiles only", Phrases: []string{"foo"}, Collections: []string{"app.bsky.actor.profile"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	hits := m.Match("did:plc:test", "app.bsky.actor.profile", nil, "foo")
	if len(hits) != 2 {
		t.Fatalf("profile: want 2 hits, got %+v", hits)
	}
	hits = m.Match("did:plc:test", "app.bsky.feed.post", nil, "foo")
	if len(hits) != 1 || hits[0].Rule != "everywhere" {
		t.Fatalf("post: want only the unscoped rule, got %+v", hits)
	}
}

func TestNewMatcherRejectsUnknownCollection(t *testing.T) {
	_, err := NewMatcher([]RuleConfig{
		{Name: "a", Phrases: []string{"x"}, Collections: []string{"app.bsky.actor.profil"}},
	})
	if err == nil {
		t.Fatal("expected error for unknown collection")
	}
}

func TestMatcherLangScope(t *testing.T) {
	m, err := NewMatcher([]RuleConfig{
		{Name: "any", Phrases: []string{"foo"}},
		{Name: "pt or ja", Phrases: []string{"foo"}, Langs: []string{"PT", "ja"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		langs []string
		want  int
	}{
		{[]string{"pt"}, 2},
		{[]string{"pt-BR"}, 2},
		{[]string{"en", "ja"}, 2},
		{[]string{"en"}, 1},
		{[]string{"ptx"}, 1}, // not a subtag of pt
		{nil, 1},             // undeclared, e.g. every profile
	} {
		if hits := m.Match("did:plc:test", "app.bsky.feed.post", tc.langs, "foo"); len(hits) != tc.want {
			t.Errorf("langs %v: want %d hits, got %+v", tc.langs, tc.want, hits)
		}
	}
}

func TestNewMatcherRejectsInvalidLang(t *testing.T) {
	_, err := NewMatcher([]RuleConfig{
		{Name: "a", Phrases: []string{"x"}, Langs: []string{"pt_BR"}},
	})
	if err == nil {
		t.Fatal("expected error for invalid language tag")
	}
}

func TestMatcherExclude(t *testing.T) {
	m, err := NewMatcher([]RuleConfig{
		{Name: "a", Phrases: []string{"cat"}, Patterns: []string{`cat\w*`}, Exclude: []string{"Category", "categorical"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, text := range []string{"Category", "CATEGORY tips", "categorical"} {
		if hits := m.Match("did:plc:test", "app.bsky.feed.post", nil, text); hits != nil {
			t.Errorf("%q: excluded text should not fire, got %+v", text, hits)
		}
	}
	// Excluding one span must not hide a real match elsewhere in the text.
	if hits := m.Match("did:plc:test", "app.bsky.feed.post", nil, "category cat"); len(hits) != 1 {
		t.Errorf("want a hit on the unexcluded cat, got %+v", hits)
	}
	// A match that runs past the excluded span still counts.
	if hits := m.Match("did:plc:test", "app.bsky.feed.post", nil, "categorycat"); len(hits) != 1 || hits[0].Matches[0] != "categorycat" {
		t.Errorf("want a pattern hit on categorycat, got %+v", hits)
	}
}

func TestMatcherExcludeKeepsContext(t *testing.T) {
	// Exclusions must not change the text other matches see: the letter
	// before "foo" is still there, so this pattern must not fire.
	m, err := NewMatcher([]RuleConfig{
		{Name: "a", Patterns: []string{`(?:^|[^\pL])foo`}, Exclude: []string{"category"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hits := m.Match("did:plc:test", "app.bsky.feed.post", nil, "categoryfoo"); hits != nil {
		t.Errorf("exclusion created a match: %+v", hits)
	}
}

func TestWithin(t *testing.T) {
	spans := [][]int{{2, 5}, {8, 10}, {10, 14}}
	for _, tc := range []struct {
		start, end int
		want       bool
	}{
		{2, 5, true}, {3, 4, true}, {1, 3, false}, {4, 6, false},
		{5, 6, false}, {8, 10, true}, {10, 12, true}, {9, 11, false},
		{14, 15, false}, {0, 1, false},
	} {
		if got := within(spans, tc.start, tc.end); got != tc.want {
			t.Errorf("within(%d, %d) = %v, want %v", tc.start, tc.end, got, tc.want)
		}
	}
	if within(nil, 0, 1) {
		t.Error("nil spans should hold nothing")
	}
}

func TestMatcherExcludeAccounts(t *testing.T) {
	m, err := NewMatcher([]RuleConfig{
		{Name: "any", Phrases: []string{"foo"}},
		{Name: "not bob", Phrases: []string{"foo"}, ExcludeAccounts: []string{" did:plc:bob "}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hits := m.Match("did:plc:bob", "app.bsky.feed.post", nil, "foo"); len(hits) != 1 || hits[0].Rule != "any" {
		t.Errorf("excluded account: want only the other rule, got %+v", hits)
	}
	if hits := m.Match("did:plc:alice", "app.bsky.feed.post", nil, "foo"); len(hits) != 2 {
		t.Errorf("other account: want 2 hits, got %+v", hits)
	}
}

func TestNewMatcherRejectsHandleInExcludeAccounts(t *testing.T) {
	_, err := NewMatcher([]RuleConfig{
		{Name: "a", Phrases: []string{"x"}, ExcludeAccounts: []string{"bob.bsky.social"}},
	})
	if err == nil {
		t.Fatal("expected error for a handle in exclude_accounts")
	}
}
