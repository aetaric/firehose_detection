package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// fakePDS issues sessions and accepts reports. The first createReport is
// rejected with ExpiredToken to exercise session refresh.
func fakePDS(t *testing.T) (*httptest.Server, *[]map[string]any) {
	var reports []map[string]any
	expired := false
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			json.NewEncoder(w).Encode(map[string]any{
				"accessJwt": "access1", "refreshJwt": "refresh1",
				"didDoc": map[string]any{"service": []map[string]string{
					{"id": "#atproto_pds", "serviceEndpoint": srv.URL},
				}},
			})
		case "/xrpc/com.atproto.server.refreshSession":
			if r.Header.Get("Authorization") != "Bearer refresh1" {
				t.Errorf("refresh used wrong token")
			}
			json.NewEncoder(w).Encode(map[string]any{"accessJwt": "access2", "refreshJwt": "refresh2"})
		case "/xrpc/com.atproto.moderation.createReport":
			if r.Header.Get("atproto-proxy") != bskyModerationProxy {
				t.Errorf("missing moderation proxy header")
			}
			if !expired {
				expired = true
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "ExpiredToken", "message": "expired"})
				return
			}
			if r.Header.Get("Authorization") != "Bearer access2" {
				t.Errorf("report used stale token %q", r.Header.Get("Authorization"))
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			reports = append(reports, body)
			json.NewEncoder(w).Encode(map[string]any{"id": 42})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reports
}

func TestReporter(t *testing.T) {
	srv, reports := fakePDS(t)
	r := NewReporter(srv.URL, "mod.example", "pw")
	ctx := context.Background()
	if err := r.Login(ctx); err != nil {
		t.Fatal(err)
	}

	id, err := r.Report(ctx, ReportSubject{
		DID: "did:plc:abc", URI: "at://did:plc:abc/app.bsky.feed.post/1", CID: "bafy", Collection: "app.bsky.feed.post",
	}, "spam", "why")
	if err != nil || id != 42 {
		t.Fatalf("post report: id=%d err=%v", id, err)
	}
	if _, err := r.Report(ctx, ReportSubject{
		DID: "did:plc:abc", URI: "at://did:plc:abc/app.bsky.actor.profile/self", Collection: "app.bsky.actor.profile",
	}, "misleading", "why"); err != nil {
		t.Fatal(err)
	}

	got := *reports
	if len(got) != 2 {
		t.Fatalf("want 2 reports, got %d", len(got))
	}
	post := got[0]["subject"].(map[string]any)
	if post["$type"] != "com.atproto.repo.strongRef" || post["cid"] != "bafy" || got[0]["reasonType"] != reportReasons["spam"] {
		t.Errorf("post report body wrong: %v", got[0])
	}
	prof := got[1]["subject"].(map[string]any)
	if prof["$type"] != "com.atproto.admin.defs#repoRef" || prof["did"] != "did:plc:abc" || got[1]["reasonType"] != reportReasons["misleading"] {
		t.Errorf("profile report body wrong: %v", got[1])
	}
}

func TestReporterRejectsUnknownReason(t *testing.T) {
	r := NewReporter("http://unused", "h", "p")
	if _, err := r.Report(context.Background(), ReportSubject{}, "bogus", ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestReportReasonsFitDiscordButtons(t *testing.T) {
	for name, token := range reportReasons {
		if !strings.Contains(token, "#reason") {
			t.Errorf("%s: malformed token %q", name, token)
		}
		row := buttonRow(name, false, false)[0].(discordgo.ActionsRow)
		btn := row.Components[0].(discordgo.Button)
		if len(btn.CustomID) > 100 || len([]rune(btn.Label)) > 80 {
			t.Errorf("%s: button id/label too long: %q / %q", name, btn.CustomID, btn.Label)
		}
	}
}
