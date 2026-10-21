package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
)

// NO MEDIA IN DISCORD. Bluesky images, video, avatars, banners and link-card
// thumbnails are treated as hazardous and must never render in the channel.
// Everything sent is built by buildEmbed/sanitizeEmbed, which only set text
// fields; message content is always empty (so Discord can't unfurl a URL into
// a preview); and record text sits in a code block so links in it are neither
// clickable nor previewed. TestNoMediaInDiscord guards this.
//
// Likewise NO LIVE LINKS from records: anything derived from a post or
// profile (text, matched terms, DID, URI) goes through codeBlock or
// inlineCode, and fields are never cut through a code span.
// TestNoLiveLinksFromRecords guards this. The only link is the embed title,
// which we build ourselves and which points at bsky.app.

const (
	idReport  = "report:" // + reason short name, e.g. report:spam
	idDismiss = "dismiss"

	fieldURI    = "AT URI"
	fieldCID    = "CID"
	fieldStatus = "Status"

	colorPost    = 0xE67E22
	colorProfile = 0x9B59B6
	colorDone    = 0x2ECC71
)

// DiscordSink posts detections to a channel with Report/Dismiss buttons.
// Emit never blocks the firehose: when Discord can't keep up, detections are
// dropped (and counted) rather than stalling the stream. The JSONL sink stays
// the lossless record.
type DiscordSink struct {
	log       *slog.Logger
	s         *discordgo.Session
	channelID string
	roleID    string    // if set, only members with this role may click
	reporter  *Reporter // nil = no buttons

	queue    chan Detection
	done     chan struct{}
	dropped  atomic.Uint64
	inflight sync.Map // message ID -> struct{}; stops double-reports
}

func NewDiscordSink(log *slog.Logger, token, channelID, roleID string, reporter *Reporter) (*DiscordSink, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuilds
	d := &DiscordSink{
		log:       log.With("component", "discord"),
		s:         s,
		channelID: channelID,
		roleID:    roleID,
		reporter:  reporter,
		queue:     make(chan Detection, 1000),
		done:      make(chan struct{}),
	}
	s.AddHandler(d.onInteraction)
	if err := s.Open(); err != nil {
		return nil, fmt.Errorf("discord connect: %w", err)
	}
	go d.worker()
	return d, nil
}

func (d *DiscordSink) Emit(det Detection) error {
	select {
	case d.queue <- det:
	default:
		d.dropped.Add(1)
	}
	return nil
}

// Flush is a no-op: Discord delivery is best-effort and asynchronous.
func (d *DiscordSink) Flush() error { return nil }

// Close drains queued messages (until ctx expires) and disconnects.
func (d *DiscordSink) Close(ctx context.Context) error {
	close(d.queue)
	select {
	case <-d.done:
	case <-ctx.Done():
		d.log.Warn("gave up draining discord queue", "remaining", len(d.queue))
	}
	return d.s.Close()
}

func (d *DiscordSink) worker() {
	defer close(d.done)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case det, ok := <-d.queue:
			if !ok {
				return
			}
			// discordgo blocks here on rate limits, which is what we want.
			if _, err := d.s.ChannelMessageSendComplex(d.channelID, buildMessage(det, d.reporter != nil)); err != nil {
				d.log.Warn("send failed", "uri", det.URI, "err", err)
			}
		case <-tick.C:
			if n := d.dropped.Swap(0); n > 0 {
				d.log.Warn("discord can't keep up; dropped detections", "count", n)
			}
		}
	}
}

func buildMessage(det Detection, buttons bool) *discordgo.MessageSend {
	msg := &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{buildEmbed(det)},
		AllowedMentions: &discordgo.MessageAllowedMentions{}, // never ping
	}
	if buttons {
		// Auto-reported detections keep only Dismiss: the report is already
		// filed, so the Report button would just duplicate it.
		_, auto := autoReportHit(det)
		msg.Components = buttonRow(reportReason(det), auto, false)
	}
	return msg
}

func buildEmbed(det Detection) *discordgo.MessageEmbed {
	title, color := "Post matched", colorPost
	link := bskyAppURL(det)
	if det.Collection == "app.bsky.actor.profile" {
		title, color = "Profile matched", colorProfile
	}
	if det.Operation == "update" {
		title += " (edited)"
	}

	// Built from whole pieces so the 1024 limit never cuts through a code
	// span, which would leave the rest of a matched URL as a live link.
	var rules []string
	for _, h := range det.Hits {
		rules = append(rules, fmt.Sprintf("**%s** (%s):", h.Rule, h.Reason))
		for _, t := range append(append([]string{}, h.Phrases...), h.Matches...) {
			rules = append(rules, inlineCode(truncate(t, 200)))
		}
	}

	media := det.Media
	if media == "" {
		media = "none"
	} else {
		media += " (not shown)"
	}

	e := &discordgo.MessageEmbed{
		Title:       truncate(title, 256),
		URL:         link,
		Color:       color,
		Description: codeBlock(det.Text, 3000),
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Rules", Value: joinWithin(rules, 1024)},
			{Name: "Account", Value: inlineCode(det.DID), Inline: true},
			{Name: "Media", Value: media, Inline: true},
			{Name: fieldURI, Value: inlineCode(det.URI)},
			{Name: fieldCID, Value: inlineCode(orDash(det.CID))},
		},
		Timestamp: time.UnixMicro(det.TimeUS).UTC().Format(time.RFC3339),
	}
	if h, ok := autoReportHit(det); ok {
		e.Color = colorDone
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  fieldStatus,
			Value: fmt.Sprintf("Auto-reported (%s) by rule %s", h.Reason, inlineCode(h.Rule)),
		})
	}
	return sanitizeEmbed(e)
}

// sanitizeEmbed strips every field of an embed that can render media. It is
// applied to everything we send, including edits of existing messages.
func sanitizeEmbed(e *discordgo.MessageEmbed) *discordgo.MessageEmbed {
	c := *e
	c.Image, c.Thumbnail, c.Video, c.Provider = nil, nil, nil, nil
	if c.Author != nil {
		a := *c.Author
		a.IconURL, a.ProxyIconURL = "", ""
		c.Author = &a
	}
	if c.Footer != nil {
		f := *c.Footer
		f.IconURL, f.ProxyIconURL = "", ""
		c.Footer = &f
	}
	c.Type = discordgo.EmbedTypeRich
	return &c
}

func buttonRow(reason string, reportDisabled, dismissDisabled bool) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{
			Label:    "Report to Bluesky (" + reason + ")",
			Style:    discordgo.DangerButton,
			CustomID: idReport + reason,
			Disabled: reportDisabled,
		},
		discordgo.Button{
			Label:    "Dismiss",
			Style:    discordgo.SecondaryButton,
			CustomID: idDismiss,
			Disabled: dismissDisabled,
		},
	}}}
}

func (d *DiscordSink) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionMessageComponent || i.Message == nil || i.Message.ChannelID != d.channelID {
		return
	}
	if i.Member == nil || (d.roleID != "" && !slices.Contains(i.Member.Roles, d.roleID)) {
		d.ephemeral(i, "You don't have the role required to action detections.")
		return
	}
	if _, busy := d.inflight.LoadOrStore(i.Message.ID, struct{}{}); busy {
		d.ephemeral(i, "Someone else is already actioning this one.")
		return
	}
	defer d.inflight.Delete(i.Message.ID)

	// Ack within Discord's 3s window; the report call can take longer.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}); err != nil {
		d.log.Warn("interaction ack failed", "err", err)
		return
	}

	user := i.Member.User
	id := i.MessageComponentData().CustomID
	switch {
	case id == idDismiss:
		// The message is gone after this, so the log line is the only audit
		// trail of who dismissed what.
		subj, _ := subjectFromMessage(i.Message)
		if err := s.ChannelMessageDelete(i.ChannelID, i.Message.ID); err != nil {
			d.log.Warn("deleting dismissed message failed", "message", i.Message.ID, "err", err)
			d.ephemeral(i, "Couldn't delete the message: "+inlineCode(err.Error()))
			return
		}
		d.log.Info("dismissed", "uri", subj.URI, "message", i.Message.ID, "by", user.Username, "by_id", user.ID)

	case strings.HasPrefix(id, idReport):
		reason := strings.TrimPrefix(id, idReport)
		subj, err := subjectFromMessage(i.Message)
		if err != nil {
			d.ephemeral(i, "Can't report: "+inlineCode(err.Error()))
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		reportID, err := d.reporter.Report(ctx, subj, reason, reportText(i.Message))
		if err != nil {
			d.log.Error("report failed", "uri", subj.URI, "err", err)
			d.ephemeral(i, "Report failed, try again: "+inlineCode(err.Error()))
			return
		}
		d.log.Info("reported", "uri", subj.URI, "reason", reason, "report_id", reportID, "by", user.Username)
		d.finish(i, fmt.Sprintf("Reported (%s) by <@%s>, Bluesky report #%d", reason, user.ID, reportID), colorDone)
	}
}

// finish stamps a status onto the message and disables its buttons.
func (d *DiscordSink) finish(i *discordgo.InteractionCreate, status string, color int) {
	var embeds []*discordgo.MessageEmbed
	if len(i.Message.Embeds) > 0 {
		e := sanitizeEmbed(i.Message.Embeds[0])
		e.Color = color
		e.Fields = append(slices.Clone(e.Fields), &discordgo.MessageEmbedField{
			Name:  fieldStatus,
			Value: status + " " + discordTime(time.Now()),
		})
		embeds = []*discordgo.MessageEmbed{e}
	}
	reason := "spam"
	if id := firstCustomID(i.Message); strings.HasPrefix(id, idReport) {
		reason = strings.TrimPrefix(id, idReport)
	}
	components := buttonRow(reason, true, true)
	empty := ""
	if _, err := d.s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:         &empty,
		Embeds:          &embeds,
		Components:      &components,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	}); err != nil {
		d.log.Warn("updating message failed", "err", err)
	}
}

// ephemeral sends a private reply to the clicker. It works both before and
// after the interaction has been acknowledged.
func (d *DiscordSink) ephemeral(i *discordgo.InteractionCreate, text string) {
	flags := discordgo.MessageFlagsEphemeral | discordgo.MessageFlagsSuppressEmbeds
	err := d.s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: text, Flags: flags},
	})
	if err != nil { // already acked: use a followup instead
		_, err = d.s.FollowupMessageCreate(i.Interaction, false, &discordgo.WebhookParams{Content: text, Flags: flags})
	}
	if err != nil {
		d.log.Warn("ephemeral reply failed", "err", err)
	}
}

// subjectFromMessage recovers what to report from the message's own embed,
// so buttons keep working across restarts with no local state.
func subjectFromMessage(m *discordgo.Message) (ReportSubject, error) {
	if len(m.Embeds) == 0 {
		return ReportSubject{}, fmt.Errorf("message has no embed")
	}
	var subj ReportSubject
	for _, f := range m.Embeds[0].Fields {
		v := strings.Trim(f.Value, "`")
		switch f.Name {
		case fieldURI:
			subj.URI = v
		case fieldCID:
			if v != "-" {
				subj.CID = v
			}
		}
	}
	rest, ok := strings.CutPrefix(subj.URI, "at://")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 {
		return ReportSubject{}, fmt.Errorf("bad AT URI %q", subj.URI)
	}
	subj.DID, subj.Collection = parts[0], parts[1]
	return subj, nil
}

// reportText is the free-text reason sent to Bluesky moderators.
func reportText(m *discordgo.Message) string {
	rules := ""
	if len(m.Embeds) > 0 {
		for _, f := range m.Embeds[0].Fields {
			if f.Name == "Rules" {
				rules = strings.NewReplacer("**", "", "`", "").Replace(f.Value)
			}
		}
	}
	return truncate("Flagged by automated detection and confirmed by a human moderator.\n"+rules, 2000)
}

func reportReason(det Detection) string {
	if h, ok := autoReportHit(det); ok {
		return h.Reason
	}
	if len(det.Hits) > 0 && det.Hits[0].Reason != "" {
		return det.Hits[0].Reason
	}
	return "spam"
}

func firstCustomID(m *discordgo.Message) string {
	for _, c := range m.Components {
		if row, ok := c.(*discordgo.ActionsRow); ok {
			for _, b := range row.Components {
				if btn, ok := b.(*discordgo.Button); ok {
					return btn.CustomID
				}
			}
		}
	}
	return ""
}

func bskyAppURL(det Detection) string {
	if det.Collection == "app.bsky.feed.post" {
		if i := strings.LastIndex(det.URI, "/"); i >= 0 {
			return "https://bsky.app/profile/" + det.DID + "/post/" + det.URI[i+1:]
		}
	}
	return "https://bsky.app/profile/" + det.DID
}

// codeBlock fences text so markdown, masked links and URLs render inert.
// Backticks inside are swapped for a lookalike so the fence can't be closed.
func codeBlock(s string, max int) string {
	s = strings.ReplaceAll(s, "`", "ˋ")
	if s == "" {
		s = " "
	}
	return "```\n" + truncate(s, max-8) + "\n```"
}

// inlineCode renders s as a single inert code span. Backticks are swapped for
// a lookalike and line breaks flattened, since either would end the span and
// let the remainder render as markdown (and URLs in it as live links).
func inlineCode(s string) string {
	return "`" + strings.NewReplacer("`", "ˋ", "\r", " ", "\n", " ").Replace(s) + "`"
}

// joinWithin joins parts with spaces (starting a new line before each **rule**
// header), stopping before the first part that would exceed max runes. Parts
// are never split.
func joinWithin(parts []string, max int) string {
	const more = " …"
	var b strings.Builder
	n := 0
	for i, p := range parts {
		sep := " "
		if strings.HasPrefix(p, "**") {
			sep = "\n"
		}
		if i == 0 {
			sep = ""
		}
		l := len([]rune(sep + p))
		if n+l > max-len([]rune(more)) {
			b.WriteString(more)
			break
		}
		b.WriteString(sep + p)
		n += l
	}
	return b.String()
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func discordTime(t time.Time) string { return fmt.Sprintf("<t:%d:R>", t.Unix()) }
