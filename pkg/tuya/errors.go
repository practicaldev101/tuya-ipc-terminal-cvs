package tuya

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError is returned by the web API calls a stream start makes
// (GetAppInfo, GetMQTTConfig, GetWebRTCConfig) when Tuya answered but did
// not accept the request: a non-200 HTTP status, or HTTP 200 with
// success=false (errorMsg such as USER_SESSION_INVALID). Error() keeps the
// upstream message text. (CVS patch)
type APIError struct {
	StatusCode int    // HTTP status; http.StatusOK for success=false
	Msg        string // errorMsg of a success=false answer
	Body       string // response body of a non-200 answer
}

func (e *APIError) Error() string {
	if e.StatusCode != http.StatusOK {
		return fmt.Sprintf("HTTP error: %d - %s", e.StatusCode, e.Body)
	}
	return e.Msg
}

// IsAPIRejection reports whether err contains an explicit rejection by the
// Tuya API: HTTP 401/403 or success=false. This is the rule the session
// keeper uses to call a session invalid; transport errors, other HTTP
// statuses (5xx, 429, ...) and unparsable bodies are no rejection. A
// rejection of a stream start is only a hint that the session may be
// invalid (success=false can also be device specific); the session keeper
// decides with its own validation call.
func IsAPIRejection(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusOK, http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return false
}
