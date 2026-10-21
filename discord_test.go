package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func testDetection() Detection {
	return Detection{
		URI:        "at://did:plc:abc/app.bsky.feed.post/3kxyz",
		CID:        "bafyrecid",
		DID:        "did:plc:abc",
		Collection: "app.bsky.feed.post",
		Operation:  "create",
		TimeUS:     1790894987429000,
		Text:       "free eth ```escape``` [click](https://evil.example/x.png) https://cdn.bsky.app/img/x.jpg",
		Media:      "2 images",
		Hits:       []RuleHit{{Rule: "crypto", Reason: "misleading", Phrases: []string{"free eth"}}},
	}
}

func TestNoMediaInDiscord(t *testing.T) {
	msg := buildMessage(testDetection(), true)
	if msg.Content != "" {
		t.Fatalf("message content must be empty so nothing unfurls, got %q", msg.Content)
	}
	if len(msg.Files) > 0 {
		t.Fatal("message must not carry attachments")
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"image"`, `"thumbnail"`, `"video"`, `"icon_url"`, `"attachments"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("message JSON contains %s: %s", key, raw)
		}
	}
	desc := msg.Embeds[0].Description
	if !strings.HasPrefix(desc, "```\n") || !strings.HasSuffix(desc, "\n```") || strings.Count(desc, "```") != 2 {
		t.Errorf("record text must sit in a single unbreakable code block: %q", desc)
	}
}

func TestSanitizeEmbedStripsMedia(t *testing.T) {
	e := sanitizeEmbed(&discordgo.MessageEmbed{
		Image:     &discordgo.MessageEmbedImage{URL: "https://x/a.png"},
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://x/b.png"},
		Video:     &discordgo.MessageEmbedVideo{URL: "https://x/c.mp4"},
		Author:    &discordgo.MessageEmbedAuthor{Name: "a", IconURL: "https://x/d.png"},
		Footer:    &discordgo.MessageEmbedFooter{Text: "f", IconURL: "https://x/e.png"},
	})
	if e.Image != nil || e.Thumbnail != nil || e.Video != nil || e.Author.IconURL != "" || e.Footer.IconURL != "" {
		t.Errorf("media survived sanitize: %+v", e)
	}
}

func TestSubjectRoundTrip(t *testing.T) {
	det := testDetection()
	msg := &discordgo.Message{Embeds: buildMessage(det, true).Embeds}
	subj, err := subjectFromMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := ReportSubject{DID: det.DID, URI: det.URI, CID: det.CID, Collection: det.Collection}
	if subj != want {
		t.Errorf("got %+v want %+v", subj, want)
	}
	if got := reportReason(det); got != "misleading" {
		t.Errorf("reason = %q", got)
	}
	if txt := reportText(msg); !strings.Contains(txt, "crypto") || strings.Contains(txt, "`") {
		t.Errorf("report text = %q", txt)
	}
}

func TestBskyAppURL(t *testing.T) {
	det := testDetection()
	if got := bskyAppURL(det); got != "https://bsky.app/profile/did:plc:abc/post/3kxyz" {
		t.Errorf("post url = %q", got)
	}
	det.Collection, det.URI = "app.bsky.actor.profile", "at://did:plc:abc/app.bsky.actor.profile/self"
	if got := bskyAppURL(det); got != "https://bsky.app/profile/did:plc:abc" {
		t.Errorf("profile url = %q", got)
	}
}

// outsideCode returns the parts of s that Discord renders as markdown, i.e.
// everything not inside a ``` block or an inline `span`.
func outsideCode(s string) string {
	var out strings.Builder
	for {
		i := strings.Index(s, "`")
		if i < 0 {
			return out.String() + s
		}
		out.WriteString(s[:i])
		fence := "`"
		if strings.HasPrefix(s[i:], "```") {
			fence = "```"
		}
		rest := s[i+len(fence):]
		j := strings.Index(rest, fence)
		if fence == "`" && strings.Contains(rest[:max(j, 0)], "\n") {
			j = -1 // Discord inline code doesn't span lines
		}
		if j < 0 {
			return out.String() + s[i:] // unclosed: Discord shows it all as markdown
		}
		s = rest[j+len(fence):]
	}
}

func TestNoLiveLinksFromRecords(t *testing.T) {
	det := testDetection()
	// Enough long URL matches to force the Rules field past its 1024 limit,
	// plus one spanning a newline.
	var urls []string
	for i := 0; i < 40; i++ {
		urls = append(urls, "https://evil.example/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	}
	urls = append(urls, "see\nhttps://evil.example/newline")
	det.Hits = append(det.Hits, RuleHit{Rule: "links", Reason: "spam", Matches: urls})

	e := buildMessage(det, true).Embeds[0]
	check := func(where, s string) {
		if strings.Contains(outsideCode(s), "://") {
			t.Errorf("%s has a URL outside code formatting:\n%s", where, s)
		}
	}
	check("description", e.Description)
	for _, f := range e.Fields {
		check("field "+f.Name, f.Value)
		if n := len([]rune(f.Value)); n > 1024 {
			t.Errorf("field %s is %d chars", f.Name, n)
		}
	}
}

func TestAutoReportedMessage(t *testing.T) {
	det := testDetection()
	det.Hits = append(det.Hits, RuleHit{Rule: "abuse", Reason: "child-safety-csam", AutoReport: true, Phrases: []string{"bad"}})
	msg := buildMessage(det, true)
	e := msg.Embeds[0]
	if e.Color != colorDone {
		t.Errorf("auto-reported embed should use the done color, got %#x", e.Color)
	}
	var status string
	for _, f := range e.Fields {
		if f.Name == fieldStatus {
			status = f.Value
		}
	}
	if !strings.Contains(status, "Auto-reported (child-safety-csam)") || !strings.Contains(status, "`abuse`") {
		t.Errorf("status field wrong: %q", status)
	}
	row := msg.Components[0].(discordgo.ActionsRow)
	report, dismiss := row.Components[0].(discordgo.Button), row.Components[1].(discordgo.Button)
	if !report.Disabled || report.CustomID != idReport+"child-safety-csam" {
		t.Errorf("report button should be disabled with the auto rule's reason: %+v", report)
	}
	if dismiss.Disabled {
		t.Error("dismiss should stay enabled")
	}

	// A manual detection is unchanged.
	plain := buildMessage(testDetection(), true)
	if plain.Embeds[0].Color == colorDone {
		t.Error("manual detection must not look reported")
	}
	if plain.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.Button).Disabled {
		t.Error("manual detection report button must be enabled")
	}
}
