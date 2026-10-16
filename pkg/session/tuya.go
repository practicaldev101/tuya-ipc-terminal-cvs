package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"

	"tuya-ipc-terminal/pkg/tuya"
)

// Production adapters talking to the Tuya web API.

// NewHTTPClient returns an HTTP client whose cookie jar holds the session's
// cookies (nil session: empty jar).
func NewHTTPClient(sessionData *tuya.SessionData) (*http.Client, error) {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}

	if sessionData != nil && len(sessionData.Cookies) > 0 {
		serverURL, err := url.Parse(fmt.Sprintf("https://%s", sessionData.ServerHost))
		if err != nil {
			return nil, err
		}
		cookies := make([]*http.Cookie, 0, len(sessionData.Cookies))
		for _, cookie := range sessionData.Cookies {
			if cookie == nil {
				continue
			}
			cookies = append(cookies, &http.Cookie{
				Name:     cookie.Name,
				Value:    cookie.Value,
				Domain:   cookie.Domain,
				Path:     cookie.Path,
				Expires:  cookie.Expires,
				Secure:   cookie.Secure,
				HttpOnly: cookie.HttpOnly,
			})
		}
		jar.SetCookies(serverURL, cookies)
	}

	return &http.Client{Timeout: 30 * time.Second, Jar: jar}, nil
}

// TuyaValidate checks a session by calling an authenticated endpoint (the
// same call `auth test` uses). Failures that are not an answer about the
// session (transport errors, HTTP status other than 200/401/403, unparsable
// body) are marked Transient.
func TuyaValidate(ctx context.Context, sessionData *tuya.SessionData) error {
	return validateWith(ctx, sessionData, http.DefaultTransport)
}

func validateWith(ctx context.Context, sessionData *tuya.SessionData, transport http.RoundTripper) error {
	if sessionData == nil {
		return fmt.Errorf("no session data")
	}
	client, err := NewHTTPClient(sessionData)
	if err != nil {
		return err
	}
	recorder := &statusRecorder{base: transport}
	client.Transport = recorder
	err = runWithContext(ctx, func() error {
		_, err := tuya.GetAppInfo(client, sessionData.ServerHost)
		return classifyValidationError(err, recorder.lastStatus())
	})
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return Transient(err)
	}
	return err
}

// classifyValidationError decides whether a failed app-info call is an
// explicit session rejection (returned as is) or a failed check (marked
// Transient). status is the last HTTP status received (0: none).
func classifyValidationError(err error, status int) error {
	if err == nil {
		return nil
	}
	var netErr net.Error
	var urlErr *url.Error
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &urlErr), errors.As(err, &netErr), errors.Is(err, io.ErrUnexpectedEOF):
		// DNS failure, timeout, connection refused/reset, TLS, ...
		return Transient(err)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return err
	case status != 0 && status != http.StatusOK:
		// 5xx, 429 and other statuses that are no verdict on the session.
		return Transient(err)
	case errors.As(err, &syntaxErr), errors.As(err, &typeErr):
		// HTTP 200 with a body that is not the API's JSON (proxy/CDN page).
		return Transient(err)
	default:
		// The API answered success=false: the session was rejected.
		if err.Error() == "" {
			return errors.New("session rejected by Tuya (success=false)")
		}
		return err
	}
}

// statusRecorder remembers the status code of the last HTTP response.
type statusRecorder struct {
	base   http.RoundTripper
	mutex  sync.Mutex
	status int
}

func (r *statusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err == nil {
		r.mutex.Lock()
		r.status = resp.StatusCode
		r.mutex.Unlock()
	}
	return resp, err
}

func (r *statusRecorder) lastStatus() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.status
}

// RegionLookup resolves a region name (e.g. "eu-central") to its endpoint.
type RegionLookup func(name string) (tuya.Region, bool)

// TuyaPasswordLogin returns a LoginFunc performing the same email/password
// login as `auth add --password`, without prompting.
func TuyaPasswordLogin(lookup RegionLookup) LoginFunc {
	return func(ctx context.Context, regionName, email, password string) (*tuya.SessionData, error) {
		region, ok := lookup(regionName)
		if !ok {
			return nil, fmt.Errorf("unknown region %q", regionName)
		}
		client, err := NewHTTPClient(nil)
		if err != nil {
			return nil, err
		}

		var loginResult *tuya.LoginResult
		err = runWithContext(ctx, func() error {
			var loginErr error
			loginResult, loginErr = tuya.PasswordLogin(client, region.Host, email, password, region.Continent)
			return loginErr
		})
		if err != nil {
			return nil, err
		}

		serverURL, _ := url.Parse(fmt.Sprintf("https://%s", region.Host))
		var cookies []*tuya.Cookie
		for _, cookie := range client.Jar.Cookies(serverURL) {
			cookies = append(cookies, &tuya.Cookie{
				Name:     cookie.Name,
				Value:    cookie.Value,
				Domain:   cookie.Domain,
				Path:     cookie.Path,
				Expires:  cookie.Expires,
				Secure:   cookie.Secure,
				HttpOnly: cookie.HttpOnly,
			})
		}
		if len(cookies) == 0 {
			return nil, fmt.Errorf("login returned no session cookies")
		}

		return &tuya.SessionData{
			LoginResult:   loginResult,
			Cookies:       cookies,
			LastValidated: time.Now(),
			ServerHost:    region.Host,
			Region:        region.Name,
			UserEmail:     loginResult.Email,
		}, nil
	}
}

// runWithContext runs fn (which has its own HTTP timeouts) but returns early
// when ctx is cancelled.
func runWithContext(ctx context.Context, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
