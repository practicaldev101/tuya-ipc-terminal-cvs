package tuya

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func apiServer(t *testing.T, status int, body string) (*http.Client, string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.Client(), strings.TrimPrefix(server.URL, "https://")
}

func TestStreamStartAPICallsReturnAPIError(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		rejection bool
		message   string
	}{
		{"success false", 200, `{"success":false,"errorMsg":"USER_SESSION_INVALID"}`, true, "USER_SESSION_INVALID"},
		{"401", 401, "denied", true, "HTTP error: 401 - denied"},
		{"403", 403, "forbidden", true, "HTTP error: 403 - forbidden"},
		{"502", 502, "bad gateway", false, "HTTP error: 502 - bad gateway"},
		{"429", 429, "slow down", false, "HTTP error: 429 - slow down"},
	}
	calls := map[string]func(*http.Client, string) error{
		"GetAppInfo":      func(c *http.Client, h string) error { _, err := GetAppInfo(c, h); return err },
		"GetMQTTConfig":   func(c *http.Client, h string) error { _, err := GetMQTTConfig(c, h); return err },
		"GetWebRTCConfig": func(c *http.Client, h string) error { _, err := GetWebRTCConfig(c, h, "dev"); return err },
	}
	for _, c := range cases {
		for name, call := range calls {
			client, host := apiServer(t, c.status, c.body)
			err := call(client, host)
			if err == nil {
				t.Fatalf("%s/%s: expected an error", c.name, name)
			}
			if err.Error() != c.message {
				t.Errorf("%s/%s: message %q, want unchanged upstream text %q", c.name, name, err.Error(), c.message)
			}
			wrapped := fmt.Errorf("failed to get app info: %w", err)
			if got := IsAPIRejection(wrapped); got != c.rejection {
				t.Errorf("%s/%s: IsAPIRejection = %v, want %v", c.name, name, got, c.rejection)
			}
		}
	}
}

func TestIsAPIRejectionIgnoresOtherErrors(t *testing.T) {
	if IsAPIRejection(nil) || IsAPIRejection(errors.New("USER_SESSION_INVALID")) {
		t.Fatal("only *APIError values are rejections")
	}
	client, host := apiServer(t, 200, "<html>proxy</html>")
	if _, err := GetAppInfo(client, host); err == nil || IsAPIRejection(err) {
		t.Fatalf("unparsable body must not be a rejection, got %v", err)
	}
}
