package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tuya-ipc-terminal/pkg/tuya"
)

func validateAgainst(t *testing.T, handler http.HandlerFunc) error {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	sd := &tuya.SessionData{ServerHost: strings.TrimPrefix(server.URL, "https://")}
	return validateWith(context.Background(), sd, server.Client().Transport)
}

func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestTuyaValidateClassifiesResponses(t *testing.T) {
	cases := []struct {
		name      string
		handler   http.HandlerFunc
		wantErr   bool
		transient bool
	}{
		{"accepted", respond(200, `{"success":true,"result":{}}`), false, false},
		{"rejected success=false", respond(200, `{"success":false,"errorMsg":"USER_SESSION_INVALID"}`), true, false},
		{"rejected without message", respond(200, `{"success":false}`), true, false},
		{"401", respond(401, "unauthorized"), true, false},
		{"403", respond(403, "forbidden"), true, false},
		{"500", respond(500, "internal error"), true, true},
		{"503", respond(503, "unavailable"), true, true},
		{"429", respond(429, "slow down"), true, true},
		{"200 non-JSON", respond(200, "<html>maintenance</html>"), true, true},
	}
	for _, c := range cases {
		err := validateAgainst(t, c.handler)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
			continue
		}
		if err != nil && IsTransient(err) != c.transient {
			t.Errorf("%s: IsTransient(%v) = %v, want %v", c.name, err, IsTransient(err), c.transient)
		}
		if err != nil && err.Error() == "" {
			t.Errorf("%s: empty error message", c.name)
		}
	}
}

func TestTuyaValidateTransportErrorsAreTransient(t *testing.T) {
	// Connection refused: a server that is already closed.
	server := httptest.NewTLSServer(respond(200, `{"success":true}`))
	host := strings.TrimPrefix(server.URL, "https://")
	transport := server.Client().Transport
	server.Close()
	err := validateWith(context.Background(), &tuya.SessionData{ServerHost: host}, transport)
	if err == nil || !IsTransient(err) {
		t.Fatalf("connection refused: %v (transient %v)", err, IsTransient(err))
	}

	// DNS failure (.invalid never resolves).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = validateWith(ctx, &tuya.SessionData{ServerHost: "tuya-session-check.invalid"}, http.DefaultTransport)
	if err == nil || !IsTransient(err) {
		t.Fatalf("DNS failure: %v (transient %v)", err, IsTransient(err))
	}

	// Timeout: the server does not answer before the context deadline.
	release := make(chan struct{})
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err = validateWith(ctx, &tuya.SessionData{ServerHost: strings.TrimPrefix(slow.URL, "https://")}, slow.Client().Transport)
	if err == nil || !IsTransient(err) {
		t.Fatalf("timeout: %v (transient %v)", err, IsTransient(err))
	}
}
