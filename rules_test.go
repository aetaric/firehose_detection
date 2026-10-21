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

	hits := m.Match("app.bsky.feed.post", "get FREE MONEY now at 0x52908400098527886E0F7030069857D2E4169EE7")
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %+v", hits)
	}
	if hits[0].Rule != "phrase" || hits[0].Phrases[0] != "free money" {
		t.Errorf("phrase hit wrong: %+v", hits[0])
	}
	if hits[1].Rule != "regex" || len(hits[1].Matches) != 1 {
		t.Errorf("regex hit wrong: %+v", hits[1])
	}

	if hits := m.Match("app.bsky.feed.post", "nothing to see here"); hits != nil {
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
		hits := m.Match("app.bsky.feed.post", text)
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
	if h := m.Match("app.bsky.feed.post", "x"); h[0].Reason != "spam" {
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
	hits := m.Match("app.bsky.feed.post", "x y")
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

	hits := m.Match("app.bsky.actor.profile", "foo")
	if len(hits) != 2 {
		t.Fatalf("profile: want 2 hits, got %+v", hits)
	}
	hits = m.Match("app.bsky.feed.post", "foo")
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

