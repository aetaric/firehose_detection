package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestAutoReportHitPicksFirstAutoRule(t *testing.T) {
	det := testDetection()
	det.Hits = []RuleHit{
		{Rule: "manual", Reason: "spam"},
		{Rule: "auto1", Reason: "child-safety-csam", AutoReport: true},
		{Rule: "auto2", Reason: "other", AutoReport: true},
	}
	h, ok := autoReportHit(det)
	if !ok || h.Rule != "auto1" {
		t.Fatalf("got %+v ok=%v, want auto1", h, ok)
	}
	if _, ok := autoReportHit(testDetection()); ok {
		t.Error("detection with no auto_report hits should not auto-report")
	}
}

func TestReportSinkFilesReportsForAutoRules(t *testing.T) {
	srv, reports := fakePDS(t)
	r := NewReporter(srv.URL, "mod.example", "pw")
	if err := r.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := NewReportSink(slog.New(slog.NewTextHandler(io.Discard, nil)), r)

	manual := testDetection()
	auto := testDetection()
	auto.Hits = append(auto.Hits, RuleHit{Rule: "abuse", Reason: "child-safety-csam", AutoReport: true, Phrases: []string{"bad"}})
	for _, d := range []Detection{manual, auto} {
		if err := s.Emit(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}

	got := *reports
	if len(got) != 1 {
		t.Fatalf("want exactly 1 report (auto only), got %d: %v", len(got), got)
	}
	if got[0]["reasonType"] != reportReasons["child-safety-csam"] {
		t.Errorf("reasonType = %v, want the auto rule's reason", got[0]["reasonType"])
	}
	text, _ := got[0]["reason"].(string)
	if !strings.Contains(text, "abuse") || strings.Contains(text, "human") {
		t.Errorf("report text should name the rule and not claim human review: %q", text)
	}
	subj := got[0]["subject"].(map[string]any)
	if subj["uri"] != auto.URI || subj["cid"] != auto.CID {
		t.Errorf("subject wrong: %v", subj)
	}
}

func TestCheckAutoReportNeedsReporter(t *testing.T) {
	auto, _ := NewMatcher([]RuleConfig{{Name: "a", Phrases: []string{"x"}, AutoReport: true}})
	manual, _ := NewMatcher([]RuleConfig{{Name: "a", Phrases: []string{"x"}}})
	r := NewReporter("http://unused", "h", "p")
	if err := checkAutoReport(auto, nil); err == nil {
		t.Error("auto_report rule without credentials must be rejected")
	}
	if err := checkAutoReport(auto, r); err != nil {
		t.Errorf("auto_report with reporter: %v", err)
	}
	if err := checkAutoReport(manual, nil); err != nil {
		t.Errorf("no auto_report rules without reporter: %v", err)
	}
}
