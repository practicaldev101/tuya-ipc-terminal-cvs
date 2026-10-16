package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"tuya-ipc-terminal/pkg/storage"
	"tuya-ipc-terminal/pkg/tuya"
)

const testUser = "eu-central_owner_at_example_com"

func TestRequestCheckIsRateLimitedPerUser(t *testing.T) {
	keeper, _, _, clock := setup(t, time.Hour, true, testCreds)

	if !keeper.RequestCheck(testUser, "USER_SESSION_INVALID") {
		t.Fatal("first request must be accepted")
	}
	if keeper.RequestCheck(testUser, "USER_SESSION_INVALID") {
		t.Fatal("second request at the same time must be rate-limited")
	}
	clock.Advance(DefaultRecheckMinInterval - time.Second)
	if keeper.RequestCheck(testUser, "USER_SESSION_INVALID") {
		t.Fatal("request within the min interval must be rate-limited")
	}
	if !keeper.RequestCheck("other_user", "USER_SESSION_INVALID") {
		t.Fatal("the rate limit is per user")
	}
	clock.Advance(time.Second)
	if !keeper.RequestCheck(testUser, "USER_SESSION_INVALID") {
		t.Fatal("request after the min interval must be accepted")
	}
}

func TestRecheckMinIntervalIsConfigurable(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	keeper := NewKeeper(Config{RecheckMinInterval: 5 * time.Second, Now: clock.Now}, &fakeStore{clock: clock}, nil, nil)
	keeper.RequestCheck(testUser, "x")
	clock.Advance(5 * time.Second)
	if !keeper.RequestCheck(testUser, "x") {
		t.Fatal("configured min interval not applied")
	}
}

// The on-demand check reuses the keeper's logic: an invalid session is
// renewed with the configured credentials and saved.
func TestOnDemandCheckRenewsInvalidSession(t *testing.T) {
	keeper, store, api, clock := setup(t, time.Hour, true, testCreds)
	keeper.CheckAll(context.Background()) // valid at startup
	stored := store.users[0].SessionData
	api.validSessions[stored] = false // another client logged in

	if _, ok := keeper.CheckPending(context.Background()); ok {
		t.Fatal("nothing requested, nothing must be checked")
	}
	keeper.RequestCheck(testUser, "USER_SESSION_INVALID")
	delay, ok := keeper.CheckPending(context.Background())
	if !ok {
		t.Fatal("requested user not checked")
	}
	if api.logins != 1 || len(store.saved) != 1 {
		t.Fatalf("logins=%d saves=%d, want 1/1", api.logins, len(store.saved))
	}
	status := keeper.Statuses()[0]
	if !status.Valid || status.Error != "" || !status.LastReloginAt.Equal(clock.Now()) {
		t.Fatalf("unexpected status after on-demand renewal: %+v", status)
	}
	if delay != DefaultCheckInterval {
		t.Fatalf("delay = %v, want the regular interval", delay)
	}
	if _, ok := keeper.CheckPending(context.Background()); ok {
		t.Fatal("pending requests must be consumed")
	}
}

func TestOnDemandCheckWithoutCredentialsOnlyReports(t *testing.T) {
	keeper, store, api, _ := setup(t, time.Hour, false, nil)
	keeper.RequestCheck(testUser, "USER_SESSION_INVALID")
	keeper.CheckPending(context.Background())
	if api.logins != 0 || len(store.saved) != 0 {
		t.Fatalf("report-only keeper re-logged in (logins=%d)", api.logins)
	}
	if status := keeper.Statuses()[0]; status.Valid || status.Error == "" {
		t.Fatalf("invalid session must be reported: %+v", status)
	}
}

func TestOnDemandCheckKeepsValidityOnTransportError(t *testing.T) {
	keeper, _, api, _ := setup(t, time.Hour, true, testCreds)
	keeper.CheckAll(context.Background())
	api.validateErr = Transient(errors.New("dial tcp: i/o timeout"))
	keeper.RequestCheck(testUser, "USER_SESSION_INVALID")
	delay, _ := keeper.CheckPending(context.Background())
	status := keeper.Statuses()[0]
	if api.logins != 0 || !status.Valid || status.CheckError == "" {
		t.Fatalf("transport error must keep validity without re-login: logins=%d %+v", api.logins, status)
	}
	if delay != DefaultCheckErrorRetryInterval {
		t.Fatalf("delay = %v, want check error retry %v", delay, DefaultCheckErrorRetryInterval)
	}
}

// A failed re-login keeps the regular retry interval: stream failures must
// not turn a wrong password into a login every RecheckMinInterval.
func TestOnDemandAfterFailedReloginWaitsForRetryInterval(t *testing.T) {
	keeper, _, api, clock := setup(t, time.Hour, false, testCreds)
	api.loginErr = errors.New("USER_PASSWD_WRONG")

	keeper.RequestCheck(testUser, "USER_SESSION_INVALID")
	delay, _ := keeper.CheckPending(context.Background())
	if api.logins != 1 || delay != DefaultRetryInterval {
		t.Fatalf("logins=%d delay=%v, want 1 / %v", api.logins, delay, DefaultRetryInterval)
	}

	clock.Advance(DefaultRecheckMinInterval)
	if keeper.RequestCheck(testUser, "USER_SESSION_INVALID") {
		t.Fatal("on-demand check accepted right after a failed re-login")
	}
	clock.Advance(DefaultRetryInterval)
	if !keeper.RequestCheck(testUser, "USER_SESSION_INVALID") {
		t.Fatal("on-demand check must be accepted once the retry interval passed")
	}
}

// countingTuya is a race-safe validate/login fake for tests that run Run.
type countingTuya struct {
	mu       sync.Mutex
	checks   int
	checked  chan struct{}
	logins   int
	rejected map[*tuya.SessionData]bool
}

func (c *countingTuya) validate(_ context.Context, sd *tuya.SessionData) error {
	c.mu.Lock()
	c.checks++
	rejected := c.rejected[sd]
	c.mu.Unlock()
	c.checked <- struct{}{}
	if rejected {
		return errors.New("USER_SESSION_INVALID")
	}
	return nil
}

func (c *countingTuya) login(context.Context, string, string, string) (*tuya.SessionData, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logins++
	return &tuya.SessionData{ServerHost: "example.invalid"}, nil
}

func (c *countingTuya) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checks, c.logins
}

type lockedStore struct {
	mu    sync.Mutex
	users []storage.UserSession
}

func (s *lockedStore) ListUsers() ([]storage.UserSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]storage.UserSession(nil), s.users...), nil
}

func (s *lockedStore) SaveUser(region, email string, sd *tuya.SessionData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.users {
		if s.users[i].Region == region && s.users[i].Email == email {
			s.users[i].SessionData = sd
			s.users[i].LastRefresh = time.Now()
		}
	}
	return nil
}

// Run serves a request immediately (not at the next 6 h tick) and a second
// request within the min interval triggers no further check.
func TestRunChecksRequestedUserImmediately(t *testing.T) {
	sd := &tuya.SessionData{ServerHost: "example.invalid"}
	store := &lockedStore{users: []storage.UserSession{{
		Region: "eu-central", Email: "owner@example.com", UserKey: testUser,
		SessionData: sd, LastRefresh: time.Now().Add(-time.Hour),
	}}}
	api := &countingTuya{checked: make(chan struct{}, 16), rejected: map[*tuya.SessionData]bool{}}
	keeper := NewKeeper(Config{Credentials: testCreds}, store, api.validate, api.login)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { keeper.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitCheck := func(what string) {
		t.Helper()
		select {
		case <-api.checked:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no check", what)
		}
	}
	waitCheck("startup")

	api.mu.Lock()
	api.rejected[sd] = true // session invalidated by another login
	api.mu.Unlock()

	if !keeper.RequestCheck(testUser, "USER_SESSION_INVALID") {
		t.Fatal("request rejected")
	}
	waitCheck("on-demand")   // the stored (rejected) session
	waitCheck("new session") // re-login validates the new session
	if keeper.RequestCheck(testUser, "USER_SESSION_INVALID") {
		t.Fatal("second request within the min interval accepted")
	}

	select {
	case <-api.checked:
		t.Fatal("unexpected extra check")
	case <-time.After(200 * time.Millisecond):
	}
	if checks, logins := api.counts(); checks != 3 || logins != 1 {
		t.Fatalf("checks=%d logins=%d, want 3/1", checks, logins)
	}
	if status := keeper.Statuses()[0]; !status.Valid || status.LastReloginAt.IsZero() {
		t.Fatalf("session not renewed: %+v", status)
	}
}
