package health

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tuya-ipc-terminal/pkg/rtsp"
	"tuya-ipc-terminal/pkg/session"
)

type fakeStreams struct {
	running bool
	streams []rtsp.StreamHealth
}

func (f fakeStreams) IsRunning() bool                    { return f.running }
func (f fakeStreams) ListenAddr() string                 { return "127.0.0.1:8554" }
func (f fakeStreams) MediaTimeout() time.Duration        { return 20 * time.Second }
func (f fakeStreams) StreamsHealth() []rtsp.StreamHealth { return f.streams }

type fakeSessions []session.Status

func (f fakeSessions) Statuses() []session.Status { return f }

func TestBuildDocument(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 30, 0, time.FixedZone("COT", -5*3600))
	streams := fakeStreams{running: true, streams: []rtsp.StreamHealth{{
		Device: "CORN_CHM-302", DeviceID: "dev1", RTSPPath: "/CORN_CHM-302", Resolution: "sd",
		State: "active", Clients: 1, StartedAt: now.Add(-time.Hour), LastMediaAt: now.Add(-2 * time.Second),
	}}}
	sessions := fakeSessions{{UserKey: "eu-central_x", Region: "eu-central", Valid: true, ExpiresAt: now.Add(48 * time.Hour)}}

	doc := Build("0.0.6-cvs.2", streams, sessions, now)

	if doc.Status != "ok" || !doc.RTSPRunning || !doc.SessionKeeper || doc.MediaTimeoutSeconds != 20 {
		t.Fatalf("unexpected document: %+v", doc)
	}
	if doc.GeneratedAt != "2026-10-16T17:00:30Z" {
		t.Fatalf("generated_at not UTC RFC3339: %s", doc.GeneratedAt)
	}
	stream := doc.Streams[0]
	if stream.LastMediaAt == nil || *stream.LastMediaAt != "2026-10-16T17:00:28Z" {
		t.Fatalf("last_media_at = %v", stream.LastMediaAt)
	}
	if stream.MediaAgeSeconds == nil || *stream.MediaAgeSeconds != 2 {
		t.Fatalf("media_age_seconds = %v", stream.MediaAgeSeconds)
	}
	if stream.LastAudioAt != nil {
		t.Fatal("zero audio time must be null")
	}
}

func TestBuildDegradedAndDown(t *testing.T) {
	now := time.Now()
	doc := Build("v", fakeStreams{running: true}, fakeSessions{{UserKey: "u", Valid: false, Error: "re-login failed"}}, now)
	if doc.Status != "degraded" {
		t.Fatalf("invalid session must degrade status, got %s", doc.Status)
	}
	doc = Build("v", fakeStreams{running: false}, nil, now)
	if doc.Status != "down" || doc.SessionKeeper {
		t.Fatalf("stopped server must be down, got %+v", doc)
	}
}

func TestHandler(t *testing.T) {
	handler := Handler("v", fakeStreams{running: true}, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET /healthz: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["streams"].([]any); !ok {
		t.Fatalf("streams must be a JSON array even when empty: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	Handler("v", fakeStreams{running: false}, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("down server status %d", rec.Code)
	}
}

func TestNormalizeAddrDefaultsToLoopback(t *testing.T) {
	cases := map[string]string{
		"8556":           "127.0.0.1:8556",
		":8556":          "127.0.0.1:8556",
		"127.0.0.1:9000": "127.0.0.1:9000",
		"0.0.0.0:8556":   "0.0.0.0:8556",
		"[::1]:8556":     "[::1]:8556",
	}
	for in, want := range cases {
		got, err := NormalizeAddr(in)
		if err != nil || got != want {
			t.Errorf("NormalizeAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeAddr(""); err == nil {
		t.Error("empty address must be rejected")
	}
}

func TestServeOnLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := Serve(ctx, "127.0.0.1:0", Handler("v", fakeStreams{running: true}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("bound to %s", addr)
	}
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("unexpected response %d %s", resp.StatusCode, body)
	}
}

// A failed validation check is reported as check_error but, with the
// previous verdict still valid, does not degrade the status.
func TestCheckErrorIsReportedWithoutDegrading(t *testing.T) {
	sessions := fakeSessions{{UserKey: "u", Valid: true, CredentialsConfigured: false, CheckError: "dial tcp: lookup example.invalid: no such host"}}
	doc := Build("v", fakeStreams{running: true}, sessions, time.Now())
	if doc.Status != "ok" {
		t.Fatalf("check error alone must not degrade, got %s", doc.Status)
	}
	raw, err := json.Marshal(doc.Sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"check_error":"dial tcp: lookup example.invalid: no such host"`) ||
		!strings.Contains(string(raw), `"credentials_configured":false`) {
		t.Fatalf("unexpected session doc: %s", raw)
	}
	raw, _ = json.Marshal(SessionDoc{UserKey: "u", Valid: true})
	if strings.Contains(string(raw), "check_error") {
		t.Fatalf("check_error must be omitted when empty: %s", raw)
	}
}

func TestStartBackoffFields(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	streams := fakeStreams{running: true, streams: []rtsp.StreamHealth{
		{Device: "cam", RTSPPath: "/cam", Resolution: "sd", State: "stopped",
			LastStartError: "failed to get app info: USER_SESSION_INVALID", NextStartAttemptAt: now.Add(4 * time.Second)},
		{Device: "cam", RTSPPath: "/cam", Resolution: "hd", State: "active", StartedAt: now},
	}}
	doc := Build("0.0.6-cvs.4", streams, nil, now)
	failed, healthy := doc.Streams[0], doc.Streams[1]
	if failed.LastStartError != "failed to get app info: USER_SESSION_INVALID" ||
		failed.NextStartAttemptAt == nil || *failed.NextStartAttemptAt != "2026-10-16T12:00:04Z" {
		t.Fatalf("backoff fields not exported: %+v", failed)
	}
	body, _ := json.Marshal(healthy)
	if strings.Contains(string(body), "last_start_error") || !strings.Contains(string(body), `"next_start_attempt_at":null`) {
		t.Fatalf("healthy stream: last_start_error must be omitted and next_start_attempt_at null: %s", body)
	}
}
