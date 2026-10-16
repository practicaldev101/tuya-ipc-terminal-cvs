package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tuya-ipc-terminal/pkg/storage"
	"tuya-ipc-terminal/pkg/tuya"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fakeStore struct {
	users []storage.UserSession
	saved []*tuya.SessionData
	clock *fakeClock
}

func (s *fakeStore) ListUsers() ([]storage.UserSession, error) {
	return append([]storage.UserSession(nil), s.users...), nil
}

func (s *fakeStore) SaveUser(region, email string, sessionData *tuya.SessionData) error {
	s.saved = append(s.saved, sessionData)
	for i := range s.users {
		if s.users[i].Region == region && s.users[i].Email == email {
			s.users[i].SessionData = sessionData
			s.users[i].LastRefresh = s.clock.Now()
		}
	}
	return nil
}

type fakeTuya struct {
	validSessions map[*tuya.SessionData]bool
	loginErr      error
	logins        int
	lastPassword  string
}

func (f *fakeTuya) validate(_ context.Context, sd *tuya.SessionData) error {
	if f.validSessions[sd] {
		return nil
	}
	return errors.New("USER_SESSION_INVALID")
}

func (f *fakeTuya) login(_ context.Context, region, email, password string) (*tuya.SessionData, error) {
	f.logins++
	f.lastPassword = password
	if f.loginErr != nil {
		return nil, f.loginErr
	}
	sd := &tuya.SessionData{Region: region, UserEmail: email, ServerHost: "example.invalid"}
	f.validSessions[sd] = true
	return sd, nil
}

func setup(t *testing.T, lastRefreshAgo time.Duration, valid bool, creds *Credentials) (*Keeper, *fakeStore, *fakeTuya, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	sd := &tuya.SessionData{ServerHost: "example.invalid"}
	store := &fakeStore{clock: clock, users: []storage.UserSession{{
		Region:      "eu-central",
		Email:       "owner@example.com",
		UserKey:     "eu-central_owner_at_example_com",
		SessionData: sd,
		LastRefresh: clock.Now().Add(-lastRefreshAgo),
	}}}
	api := &fakeTuya{validSessions: map[*tuya.SessionData]bool{sd: valid}}
	keeper := NewKeeper(Config{Credentials: creds, Now: clock.Now}, store, api.validate, api.login)
	return keeper, store, api, clock
}

var testCreds = &Credentials{Email: "Owner@Example.com", Password: "pw"}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	margin := 24 * time.Hour
	cases := []struct {
		name      string
		expiresAt time.Time
		err       error
		want      Decision
	}{
		{"fresh", now.Add(72 * time.Hour), nil, DecisionKeep},
		{"just outside margin", now.Add(24*time.Hour + time.Second), nil, DecisionKeep},
		{"at margin", now.Add(24 * time.Hour), nil, DecisionRenewExpiring},
		{"inside margin", now.Add(time.Hour), nil, DecisionRenewExpiring},
		{"expired", now.Add(-time.Second), nil, DecisionRenewInvalid},
		{"unknown expiry", time.Time{}, nil, DecisionRenewInvalid},
		{"rejected by tuya", now.Add(72 * time.Hour), errors.New("invalid"), DecisionRenewInvalid},
	}
	for _, c := range cases {
		if got := Decide(now, c.expiresAt, margin, c.err); got != c.want {
			t.Errorf("%s: Decide = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestExpiresAtPrefersCookieExpiry(t *testing.T) {
	refresh := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cookieExpiry := refresh.Add(48 * time.Hour)
	user := storage.UserSession{LastRefresh: refresh, SessionData: &tuya.SessionData{Cookies: []*tuya.Cookie{
		{Name: "a"}, {Name: "b", Expires: cookieExpiry.Add(time.Hour)}, {Name: "c", Expires: cookieExpiry},
	}}}
	if got := ExpiresAt(user, 96*time.Hour); !got.Equal(cookieExpiry) {
		t.Fatalf("ExpiresAt = %v, want earliest cookie expiry %v", got, cookieExpiry)
	}
	user.SessionData.Cookies = []*tuya.Cookie{{Name: "a"}}
	if got := ExpiresAt(user, 96*time.Hour); !got.Equal(refresh.Add(96 * time.Hour)) {
		t.Fatalf("ExpiresAt without cookie expiry = %v, want LastRefresh+lifetime", got)
	}
}

func TestFreshValidSessionIsKeptAndNextCheckIsInterval(t *testing.T) {
	keeper, store, api, _ := setup(t, time.Hour, true, testCreds)

	next := keeper.CheckAll(context.Background())

	if api.logins != 0 || len(store.saved) != 0 {
		t.Fatalf("unexpected re-login (%d) / save (%d)", api.logins, len(store.saved))
	}
	if next != DefaultCheckInterval {
		t.Fatalf("next check in %v, want %v", next, DefaultCheckInterval)
	}
	statuses := keeper.Statuses()
	if len(statuses) != 1 || !statuses[0].Valid || !statuses[0].CredentialsConfigured {
		t.Fatalf("unexpected status %+v", statuses)
	}
}

func TestNextCheckWakesUpAtRenewMargin(t *testing.T) {
	// Refreshed 70h ago: expiry at 96h, renew margin reached at 72h -> in 2h,
	// earlier than the 6h interval.
	keeper, _, _, _ := setup(t, 70*time.Hour, true, testCreds)
	if next := keeper.CheckAll(context.Background()); next != 2*time.Hour {
		t.Fatalf("next check in %v, want 2h", next)
	}
}

func TestExpiringSessionIsRenewedProactively(t *testing.T) {
	keeper, store, api, clock := setup(t, 80*time.Hour, true, testCreds)

	keeper.CheckAll(context.Background())

	if api.logins != 1 || len(store.saved) != 1 {
		t.Fatalf("logins=%d saves=%d, want 1/1", api.logins, len(store.saved))
	}
	if api.lastPassword != "pw" {
		t.Fatal("configured password not used")
	}
	status := keeper.Statuses()[0]
	if !status.Valid || status.Error != "" || !status.LastReloginAt.Equal(clock.Now()) {
		t.Fatalf("unexpected status after renewal: %+v", status)
	}
	if want := clock.Now().Add(DefaultSessionLifetime); !status.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at %v, want %v", status.ExpiresAt, want)
	}

	// Next round with the renewed session: nothing to do.
	clock.Advance(6 * time.Hour)
	keeper.CheckAll(context.Background())
	if api.logins != 1 {
		t.Fatalf("renewed session re-logged in again (logins=%d)", api.logins)
	}
}

func TestInvalidSessionIsRenewedEvenIfYoung(t *testing.T) {
	keeper, store, api, _ := setup(t, time.Hour, false, testCreds)
	keeper.CheckAll(context.Background())
	if api.logins != 1 || len(store.saved) != 1 {
		t.Fatalf("logins=%d saves=%d, want 1/1", api.logins, len(store.saved))
	}
	if !keeper.Statuses()[0].Valid {
		t.Fatal("status not valid after successful re-login")
	}
}

func TestReloginFailureKeepsRunningAndRetriesSooner(t *testing.T) {
	keeper, store, api, _ := setup(t, time.Hour, false, testCreds)
	api.loginErr = errors.New("ILLEGAL_PASSWORD")

	next := keeper.CheckAll(context.Background())

	if len(store.saved) != 0 {
		t.Fatal("failed login must not overwrite the stored session")
	}
	if next != DefaultRetryInterval {
		t.Fatalf("next check in %v, want retry interval %v", next, DefaultRetryInterval)
	}
	status := keeper.Statuses()[0]
	if status.Valid || status.Error == "" {
		t.Fatalf("unexpected status after failed re-login: %+v", status)
	}
}

func TestExpiringSessionWithFailedReloginDoesNotSpin(t *testing.T) {
	keeper, _, api, _ := setup(t, 90*time.Hour, true, testCreds)
	api.loginErr = errors.New("network down")
	if next := keeper.CheckAll(context.Background()); next != DefaultRetryInterval {
		t.Fatalf("next check in %v, want retry interval %v", next, DefaultRetryInterval)
	}
}

func TestNoCredentialsReportsWithoutLogin(t *testing.T) {
	keeper, store, api, _ := setup(t, 80*time.Hour, true, nil)

	next := keeper.CheckAll(context.Background())

	if api.logins != 0 || len(store.saved) != 0 {
		t.Fatal("must not try to log in without credentials")
	}
	if next != DefaultCheckInterval {
		t.Fatalf("next check in %v, want %v", next, DefaultCheckInterval)
	}
	status := keeper.Statuses()[0]
	if status.CredentialsConfigured || status.Error == "" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestCredentialsForOtherAccountAreNotUsed(t *testing.T) {
	keeper, _, api, _ := setup(t, time.Hour, false, &Credentials{Email: "someone-else@example.com", Password: "x"})
	keeper.CheckAll(context.Background())
	if api.logins != 0 {
		t.Fatal("credentials of another account were used")
	}
}

func TestCredentialsRegionMustMatch(t *testing.T) {
	creds := &Credentials{Email: "owner@example.com", Password: "x", Region: "us-west"}
	if creds.Matches("eu-central", "owner@example.com") {
		t.Fatal("region mismatch must not match")
	}
	creds.Region = ""
	if !creds.Matches("eu-central", "OWNER@example.com") {
		t.Fatal("email match should be case-insensitive when region unset")
	}
}

func TestLoadCredentials(t *testing.T) {
	env := map[string]string{EnvEmail: "env@example.com", EnvPassword: "envpw"}
	getenv := func(k string) string { return env[k] }

	creds, err := LoadCredentials("", getenv)
	if err != nil || creds == nil || creds.Email != "env@example.com" || creds.Password != "envpw" {
		t.Fatalf("env credentials: %+v %v", creds, err)
	}

	path := filepath.Join(t.TempDir(), "tuya.env")
	content := "# comment\nexport TUYA_EMAIL=file@example.com\nTUYA_PASSWORD=\"p=w d\"\nTUYA_REGION='eu-central'\nOTHER=ignored\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	creds, err = LoadCredentials(path, getenv)
	if err != nil || creds.Email != "file@example.com" || creds.Password != "p=w d" || creds.Region != "eu-central" {
		t.Fatalf("file credentials: %+v %v", creds, err)
	}

	creds, err = LoadCredentials("", func(string) string { return "" })
	if err != nil || creds != nil {
		t.Fatalf("no credentials should yield nil, got %+v %v", creds, err)
	}

	if _, err := LoadCredentials("", func(k string) string {
		if k == EnvEmail {
			return "only@example.com"
		}
		return ""
	}); !errors.Is(err, ErrIncompleteCredentials) {
		t.Fatalf("email without password must be ErrIncompleteCredentials, got %v", err)
	}

	if _, err := LoadCredentials(filepath.Join(t.TempDir(), "missing"), getenv); err == nil {
		t.Fatal("missing credentials file must be an error")
	}
}
