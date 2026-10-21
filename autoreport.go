package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// ReportSink files Bluesky reports, with no human in the loop, for detections
// where a rule marked auto_report fired. Like the Discord sink it never
// blocks the firehose: reports are queued and filed by a worker, and dropped
// (and counted) if the queue is full. The JSONL sink stays the lossless
// record; its hits carry auto_report so dropped ones can be found.
type ReportSink struct {
	log      *slog.Logger
	reporter *Reporter

	queue   chan Detection
	done    chan struct{}
	dropped atomic.Uint64
}

func NewReportSink(log *slog.Logger, reporter *Reporter) *ReportSink {
	s := &ReportSink{
		log:      log.With("component", "autoreport"),
		reporter: reporter,
		queue:    make(chan Detection, 1000),
		done:     make(chan struct{}),
	}
	go s.worker()
	return s
}

func (s *ReportSink) Emit(det Detection) error {
	if _, ok := autoReportHit(det); !ok {
		return nil
	}
	select {
	case s.queue <- det:
	default:
		s.dropped.Add(1)
	}
	return nil
}

// Flush is a no-op: reports are filed asynchronously.
func (s *ReportSink) Flush() error { return nil }

// Close drains queued reports (until ctx expires).
func (s *ReportSink) Close(ctx context.Context) error {
	close(s.queue)
	select {
	case <-s.done:
	case <-ctx.Done():
		s.log.Warn("gave up draining auto-report queue", "remaining", len(s.queue))
	}
	return nil
}

func (s *ReportSink) worker() {
	defer close(s.done)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case det, ok := <-s.queue:
			if !ok {
				return
			}
			s.file(det)
		case <-tick.C:
			if n := s.dropped.Swap(0); n > 0 {
				s.log.Warn("auto-report can't keep up; dropped detections", "count", n)
			}
		}
	}
}

func (s *ReportSink) file(det Detection) {
	hit, _ := autoReportHit(det)
	subj := ReportSubject{DID: det.DID, URI: det.URI, CID: det.CID, Collection: det.Collection}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := s.reporter.Report(ctx, subj, hit.Reason, autoReportText(det))
	if err != nil {
		s.log.Error("auto-report failed", "uri", det.URI, "rule", hit.Rule, "err", err)
		return
	}
	s.log.Info("auto-reported", "uri", det.URI, "rule", hit.Rule, "reason", hit.Reason, "report_id", id)
}

// autoReportHit returns the first hit (in rules-file order) whose rule has
// auto_report set. That rule's reason is the one filed.
func autoReportHit(det Detection) (RuleHit, bool) {
	for _, h := range det.Hits {
		if h.AutoReport {
			return h, true
		}
	}
	return RuleHit{}, false
}

// autoReportText is the free-text reason sent to Bluesky moderators for an
// automatic report. It must not claim anyone looked at it.
func autoReportText(det Detection) string {
	var b strings.Builder
	b.WriteString("Filed automatically by detection rules; no moderator review.\n")
	for _, h := range det.Hits {
		fmt.Fprintf(&b, "%s (%s): %s\n", h.Rule, h.Reason, strings.Join(append(append([]string{}, h.Phrases...), h.Matches...), " "))
	}
	return truncate(b.String(), 2000)
}

// checkAutoReport refuses a rule set that asks for automatic reports when no
// Bluesky credentials are configured. Starting (or reloading) anyway would
// silently file nothing.
func checkAutoReport(m *Matcher, reporter *Reporter) error {
	if m.HasAutoReport() && reporter == nil {
		return fmt.Errorf("rules use auto_report but BSKY_HANDLE/BSKY_APP_PASSWORD are unset")
	}
	return nil
}
