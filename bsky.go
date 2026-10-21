package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// bskyModerationProxy routes createReport to Bluesky's official moderation
// service via the account's PDS.
const bskyModerationProxy = "did:plc:ar7c4by46qjdydhdevvrndac#atproto_labeler"

// reportReasons maps the short names used in rules.yaml to lexicon tokens.
// The first six are the original com.atproto reasons; the rest are the more
// specific tools.ozone reasons, named by kebab-casing the lexicon suffix
// (reasonChildSafetyCSAM -> child-safety-csam).
var reportReasons = map[string]string{
	"spam":       "com.atproto.moderation.defs#reasonSpam",
	"violation":  "com.atproto.moderation.defs#reasonViolation",
	"misleading": "com.atproto.moderation.defs#reasonMisleading",
	"sexual":     "com.atproto.moderation.defs#reasonSexual",
	"rude":       "com.atproto.moderation.defs#reasonRude",
	"other":      "com.atproto.moderation.defs#reasonOther",

	"violence-animal":            "tools.ozone.report.defs#reasonViolenceAnimal",
	"violence-threats":           "tools.ozone.report.defs#reasonViolenceThreats",
	"violence-graphic-content":   "tools.ozone.report.defs#reasonViolenceGraphicContent",
	"violence-glorification":     "tools.ozone.report.defs#reasonViolenceGlorification",
	"violence-extremist-content": "tools.ozone.report.defs#reasonViolenceExtremistContent",
	"violence-trafficking":       "tools.ozone.report.defs#reasonViolenceTrafficking",
	"violence-other":             "tools.ozone.report.defs#reasonViolenceOther",

	"sexual-abuse-content": "tools.ozone.report.defs#reasonSexualAbuseContent",
	"sexual-ncii":          "tools.ozone.report.defs#reasonSexualNCII",
	"sexual-deepfake":      "tools.ozone.report.defs#reasonSexualDeepfake",
	"sexual-animal":        "tools.ozone.report.defs#reasonSexualAnimal",
	"sexual-unlabeled":     "tools.ozone.report.defs#reasonSexualUnlabeled",
	"sexual-other":         "tools.ozone.report.defs#reasonSexualOther",

	"child-safety-csam":       "tools.ozone.report.defs#reasonChildSafetyCSAM",
	"child-safety-groom":      "tools.ozone.report.defs#reasonChildSafetyGroom",
	"child-safety-privacy":    "tools.ozone.report.defs#reasonChildSafetyPrivacy",
	"child-safety-harassment": "tools.ozone.report.defs#reasonChildSafetyHarassment",
	"child-safety-other":      "tools.ozone.report.defs#reasonChildSafetyOther",

	"harassment-troll":       "tools.ozone.report.defs#reasonHarassmentTroll",
	"harassment-targeted":    "tools.ozone.report.defs#reasonHarassmentTargeted",
	"harassment-hate-speech": "tools.ozone.report.defs#reasonHarassmentHateSpeech",
	"harassment-doxxing":     "tools.ozone.report.defs#reasonHarassmentDoxxing",
	"harassment-other":       "tools.ozone.report.defs#reasonHarassmentOther",

	"misleading-bot":           "tools.ozone.report.defs#reasonMisleadingBot",
	"misleading-impersonation": "tools.ozone.report.defs#reasonMisleadingImpersonation",
	"misleading-spam":          "tools.ozone.report.defs#reasonMisleadingSpam",
	"misleading-scam":          "tools.ozone.report.defs#reasonMisleadingScam",
	"misleading-elections":     "tools.ozone.report.defs#reasonMisleadingElections",
	"misleading-other":         "tools.ozone.report.defs#reasonMisleadingOther",

	"rule-site-security":    "tools.ozone.report.defs#reasonRuleSiteSecurity",
	"rule-prohibited-sales": "tools.ozone.report.defs#reasonRuleProhibitedSales",
	"rule-ban-evasion":      "tools.ozone.report.defs#reasonRuleBanEvasion",
	"rule-other":            "tools.ozone.report.defs#reasonRuleOther",

	"self-harm-content":    "tools.ozone.report.defs#reasonSelfHarmContent",
	"self-harm-ed":         "tools.ozone.report.defs#reasonSelfHarmED",
	"self-harm-stunts":     "tools.ozone.report.defs#reasonSelfHarmStunts",
	"self-harm-substances": "tools.ozone.report.defs#reasonSelfHarmSubstances",
	"self-harm-other":      "tools.ozone.report.defs#reasonSelfHarmOther",
}

// ReportSubject identifies what is being reported. Profiles are reported as
// the whole account (repoRef); anything else as the specific record
// (strongRef), which requires a CID.
type ReportSubject struct {
	DID        string
	URI        string
	CID        string
	Collection string
}

// Reporter files moderation reports as a Bluesky account using an app
// password. It is safe for concurrent use.
type Reporter struct {
	entryway string
	handle   string
	password string
	http     *http.Client

	mu      sync.Mutex
	pds     string // resolved from the session's DID doc; falls back to entryway
	access  string
	refresh string
}

func NewReporter(entryway, handle, password string) *Reporter {
	return &Reporter{
		entryway: strings.TrimRight(entryway, "/"),
		handle:   handle,
		password: password,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

type xrpcError struct {
	Status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *xrpcError) Error() string {
	return fmt.Sprintf("xrpc %d %s: %s", e.Status, e.Code, e.Message)
}

func (e *xrpcError) authExpired() bool {
	return e.Status == http.StatusUnauthorized || e.Code == "ExpiredToken" || e.Code == "InvalidToken"
}

type session struct {
	AccessJwt  string          `json:"accessJwt"`
	RefreshJwt string          `json:"refreshJwt"`
	DIDDoc     json.RawMessage `json:"didDoc"`
}

// Login creates a fresh session. Call it at startup to fail fast on bad
// credentials.
func (r *Reporter) Login(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loginLocked(ctx)
}

func (r *Reporter) loginLocked(ctx context.Context) error {
	var s session
	body := map[string]string{"identifier": r.handle, "password": r.password}
	if err := r.call(ctx, r.entryway, "com.atproto.server.createSession", "", nil, body, &s); err != nil {
		return fmt.Errorf("bluesky login: %w", err)
	}
	r.setSession(s)
	return nil
}

func (r *Reporter) setSession(s session) {
	r.access, r.refresh = s.AccessJwt, s.RefreshJwt
	r.pds = r.entryway
	if ep := pdsEndpoint(s.DIDDoc); ep != "" {
		r.pds = ep
	}
}

// renew refreshes the session, falling back to a full login if the refresh
// token is also dead.
func (r *Reporter) renew(ctx context.Context, staleAccess string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.access != staleAccess {
		return nil // another goroutine already renewed
	}
	var s session
	if err := r.call(ctx, r.pds, "com.atproto.server.refreshSession", r.refresh, nil, nil, &s); err == nil {
		r.setSession(s)
		return nil
	}
	return r.loginLocked(ctx)
}

// Report files a report and returns the report ID assigned by the
// moderation service.
func (r *Reporter) Report(ctx context.Context, subj ReportSubject, reason, text string) (int64, error) {
	reasonType, ok := reportReasons[reason]
	if !ok {
		return 0, fmt.Errorf("unknown report reason %q", reason)
	}
	var subject map[string]string
	if subj.Collection == "app.bsky.actor.profile" {
		subject = map[string]string{"$type": "com.atproto.admin.defs#repoRef", "did": subj.DID}
	} else {
		if subj.CID == "" {
			return 0, errors.New("record report needs a CID")
		}
		subject = map[string]string{"$type": "com.atproto.repo.strongRef", "uri": subj.URI, "cid": subj.CID}
	}
	body := map[string]any{"reasonType": reasonType, "reason": text, "subject": subject}
	hdr := map[string]string{"atproto-proxy": bskyModerationProxy}

	var out struct {
		ID int64 `json:"id"`
	}
	for attempt := 0; ; attempt++ {
		r.mu.Lock()
		pds, access := r.pds, r.access
		r.mu.Unlock()
		if access == "" {
			if err := r.Login(ctx); err != nil {
				return 0, err
			}
			continue
		}
		err := r.call(ctx, pds, "com.atproto.moderation.createReport", access, hdr, body, &out)
		var xe *xrpcError
		if attempt == 0 && errors.As(err, &xe) && xe.authExpired() {
			if err := r.renew(ctx, access); err != nil {
				return 0, err
			}
			continue
		}
		return out.ID, err
	}
}

// call POSTs an XRPC procedure. body may be nil.
func (r *Reporter) call(ctx context.Context, base, nsid, token string, hdr map[string]string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/xrpc/"+nsid, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		xe := &xrpcError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, xe)
		return xe
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// pdsEndpoint extracts the #atproto_pds service URL from a DID document.
func pdsEndpoint(raw json.RawMessage) string {
	var doc struct {
		Service []struct {
			ID              string `json:"id"`
			ServiceEndpoint string `json:"serviceEndpoint"`
		} `json:"service"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	for _, s := range doc.Service {
		if strings.HasSuffix(s.ID, "#atproto_pds") {
			return strings.TrimRight(s.ServiceEndpoint, "/")
		}
	}
	return ""
}
