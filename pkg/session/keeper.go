// Package session keeps stored Tuya web sessions alive (CVS patch).
//
// The bridge authenticates once (QR code or email/password) and stores the
// session cookies under .tuya-data. The cookies expire after roughly four
// days and nothing in upstream renews them, so streams silently stop
// starting. The Keeper periodically validates every stored session and, when
// it is invalid or close to its (stored or assumed) expiry, re-logs in
// non-interactively with operator-supplied credentials.
//
// Only an explicit rejection by Tuya marks a session invalid. A check that
// fails for transport or server reasons (DNS, timeout, connection refused,
// HTTP 5xx, ...) says nothing about the session: the previous validity is
// kept, the failure is reported as check_error and the check is retried
// after CheckErrorRetryInterval.
//
// Besides the schedule, RequestCheck asks Run to check one user right away
// (e.g. when Tuya rejected a stream start with USER_SESSION_INVALID). These
// on-demand checks run the same validation / re-login logic, are serialized
// with the scheduled checks and are rate-limited per user.
package session

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"tuya-ipc-terminal/pkg/core"
	"tuya-ipc-terminal/pkg/storage"
	"tuya-ipc-terminal/pkg/tuya"
)

// Defaults for Config.
const (
	DefaultCheckInterval   = 6 * time.Hour
	DefaultRenewMargin     = 24 * time.Hour
	DefaultSessionLifetime = 96 * time.Hour // observed Tuya web cookie lifetime (~4 days)
	DefaultRetryInterval   = 30 * time.Minute
	// DefaultCheckErrorRetryInterval: next check after a validation that
	// failed for transport/server reasons.
	DefaultCheckErrorRetryInterval = 5 * time.Minute
	// DefaultRecheckMinInterval: minimum time between two on-demand checks
	// of the same user (RequestCheck).
	DefaultRecheckMinInterval = 30 * time.Second
	minDelay                  = time.Minute
)

// Store is the subset of storage.StorageManager used by the keeper.
type Store interface {
	ListUsers() ([]storage.UserSession, error)
	SaveUser(region, email string, sessionData *tuya.SessionData) error
}

// ValidateFunc returns nil when the stored session is accepted by Tuya. An
// error marked with Transient means the check itself failed (transport or
// server error) and says nothing about the session; any other error is an
// explicit rejection of the session.
type ValidateFunc func(ctx context.Context, sessionData *tuya.SessionData) error

type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

// Transient marks err as a failed check (not a verdict on the session).
func Transient(err error) error {
	if err == nil || IsTransient(err) {
		return err
	}
	return &transientError{err: err}
}

// IsTransient reports whether err is a failed check rather than a session
// rejection. Context cancellation and deadline errors count as transient.
func IsTransient(err error) bool {
	var transient *transientError
	return errors.As(err, &transient) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// LoginFunc performs a non-interactive email/password login.
type LoginFunc func(ctx context.Context, region, email, password string) (*tuya.SessionData, error)

// Config configures a Keeper.
type Config struct {
	CheckInterval   time.Duration // how often sessions are validated
	RenewMargin     time.Duration // re-login this long before expiry
	SessionLifetime time.Duration // assumed lifetime when cookies carry no expiry
	RetryInterval   time.Duration // next check after a failed re-login
	// CheckErrorRetryInterval is the next check after a validation that
	// failed for transport/server reasons.
	CheckErrorRetryInterval time.Duration
	// RecheckMinInterval is the minimum time between two on-demand checks
	// (RequestCheck) of the same user.
	RecheckMinInterval time.Duration
	Credentials        *Credentials // nil: cannot re-login, only report
	Now                func() time.Time
}

// Status is the last known state of one stored user session.
type Status struct {
	UserKey               string    `json:"user_key"`
	Region                string    `json:"region"`
	Valid                 bool      `json:"valid"`
	CheckedAt             time.Time `json:"checked_at"`
	ExpiresAt             time.Time `json:"expires_at"`
	LastRefresh           time.Time `json:"last_refresh"`
	LastReloginAt         time.Time `json:"last_relogin_at,omitempty"`
	CredentialsConfigured bool      `json:"credentials_configured"`
	Error                 string    `json:"error,omitempty"`
	// CheckError is set when the last validation could not be completed
	// (transport/server error); Valid and Error then carry the previous
	// result.
	CheckError string `json:"check_error,omitempty"`

	// reloginFailed: this check attempted a re-login and it failed.
	reloginFailed bool
}

// Keeper validates and renews stored sessions.
type Keeper struct {
	cfg      Config
	store    Store
	validate ValidateFunc
	login    LoginFunc

	mutex    sync.RWMutex
	statuses map[string]Status

	// On-demand checks (RequestCheck -> Run).
	wake        chan struct{} // capacity 1: "pending is not empty"
	reqMutex    sync.Mutex
	pending     map[string]string    // user key -> reason
	lastRequest map[string]time.Time // user key -> last accepted request
}

// NewKeeper builds a Keeper; zero (or negative) Config durations take the
// defaults.
func NewKeeper(cfg Config, store Store, validate ValidateFunc, login LoginFunc) *Keeper {
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = DefaultCheckInterval
	}
	if cfg.RenewMargin <= 0 {
		cfg.RenewMargin = DefaultRenewMargin
	}
	if cfg.SessionLifetime <= 0 {
		cfg.SessionLifetime = DefaultSessionLifetime
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = DefaultRetryInterval
	}
	if cfg.CheckErrorRetryInterval <= 0 {
		cfg.CheckErrorRetryInterval = DefaultCheckErrorRetryInterval
	}
	if cfg.RecheckMinInterval <= 0 {
		cfg.RecheckMinInterval = DefaultRecheckMinInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Keeper{
		cfg:         cfg,
		store:       store,
		validate:    validate,
		login:       login,
		statuses:    make(map[string]Status),
		wake:        make(chan struct{}, 1),
		pending:     make(map[string]string),
		lastRequest: make(map[string]time.Time),
	}
}

// ExpiresAt returns when a stored session is expected to expire: the earliest
// cookie expiry when the cookies carry one, otherwise LastRefresh plus the
// assumed lifetime. A session without LastRefresh is treated as expired.
func ExpiresAt(user storage.UserSession, lifetime time.Duration) time.Time {
	var earliest time.Time
	if user.SessionData != nil {
		for _, cookie := range user.SessionData.Cookies {
			if cookie == nil || cookie.Expires.IsZero() {
				continue
			}
			if earliest.IsZero() || cookie.Expires.Before(earliest) {
				earliest = cookie.Expires
			}
		}
	}
	if !earliest.IsZero() {
		return earliest
	}
	if user.LastRefresh.IsZero() {
		return time.Time{}
	}
	return user.LastRefresh.Add(lifetime)
}

// Decision is the outcome of evaluating one session.
type Decision int

const (
	// DecisionKeep: session valid and not close to expiry.
	DecisionKeep Decision = iota
	// DecisionRenewExpiring: valid but within the renew margin of expiry.
	DecisionRenewExpiring
	// DecisionRenewInvalid: Tuya rejected the session (or it is expired).
	DecisionRenewInvalid
)

// Decide evaluates a session given the validation result. It is pure so the
// scheduling rules can be unit-tested without a clock or network.
func Decide(now, expiresAt time.Time, margin time.Duration, validationErr error) Decision {
	if validationErr != nil {
		return DecisionRenewInvalid
	}
	if expiresAt.IsZero() || !now.Before(expiresAt) {
		return DecisionRenewInvalid
	}
	if !now.Before(expiresAt.Add(-margin)) {
		return DecisionRenewExpiring
	}
	return DecisionKeep
}

// Run checks all sessions immediately and then on schedule until ctx ends.
// Users passed to RequestCheck are checked as soon as possible in between;
// all checks run on this goroutine, so they never overlap.
func (k *Keeper) Run(ctx context.Context) {
	delay := k.CheckAll(ctx)
	deadline := time.Now().Add(delay)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			delay = k.CheckAll(ctx)
			deadline = time.Now().Add(delay)
			timer.Reset(delay)
		case <-k.wake:
			// An on-demand check that needs an earlier retry (failed
			// re-login, failed check) pulls the next scheduled check in.
			if delay, ok := k.CheckPending(ctx); ok && delay < time.Until(deadline) {
				deadline = time.Now().Add(delay)
				timer.Reset(delay)
			}
		}
	}
}

// RequestCheck asks Run to validate (and if needed renew) the session of
// userKey now instead of at the next scheduled check, e.g. because Tuya
// rejected a stream start. It never blocks. A request is dropped (false)
// when the same user was requested less than RecheckMinInterval ago, or
// when its last check ended in a failed re-login less than RetryInterval
// ago (the regular retry of a failed re-login is not shortened, so a wrong
// password cannot turn into a login loop).
func (k *Keeper) RequestCheck(userKey, reason string) bool {
	now := k.cfg.Now()
	log := core.Logger.With().Str("component", "session").Str("user", userKey).Str("reason", reason).Logger()

	if status, ok := k.getStatus(userKey); ok && status.reloginFailed && now.Sub(status.CheckedAt) < k.cfg.RetryInterval {
		log.Debug().Msg("session: on-demand check skipped, last re-login failed recently")
		return false
	}

	k.reqMutex.Lock()
	if last, ok := k.lastRequest[userKey]; ok && now.Sub(last) < k.cfg.RecheckMinInterval {
		k.reqMutex.Unlock()
		log.Debug().Dur("min_interval", k.cfg.RecheckMinInterval).Msg("session: on-demand check rate-limited")
		return false
	}
	k.lastRequest[userKey] = now
	k.pending[userKey] = reason
	k.reqMutex.Unlock()

	select {
	case k.wake <- struct{}{}:
	default: // a wake-up is already queued; it drains all pending users
	}
	log.Warn().Msg("session: on-demand check requested")
	return true
}

// CheckPending checks the users requested through RequestCheck (Run calls
// it; exported for tests). ok is false when nothing was checked; otherwise
// delay is the earliest next check those results ask for.
func (k *Keeper) CheckPending(ctx context.Context) (delay time.Duration, ok bool) {
	k.reqMutex.Lock()
	pending := k.pending
	k.pending = make(map[string]string)
	k.reqMutex.Unlock()
	if len(pending) == 0 {
		return 0, false
	}

	users, err := k.store.ListUsers()
	if err != nil {
		core.Logger.Error().Err(err).Str("component", "session").Msg("session: cannot list stored users")
		return k.cfg.RetryInterval, true
	}
	next := k.cfg.CheckInterval
	for _, user := range users {
		reason, requested := pending[user.UserKey]
		if !requested || ctx.Err() != nil {
			continue
		}
		core.Logger.Info().Str("component", "session").Str("user", user.UserKey).Str("reason", reason).
			Msg("session: on-demand check")
		next = k.checkAndRecord(ctx, user, next)
		ok = true
	}
	if next < minDelay {
		next = minDelay
	}
	return next, ok
}

// CheckAll validates (and if needed renews) every stored session once and
// returns the delay until the next check.
func (k *Keeper) CheckAll(ctx context.Context) time.Duration {
	users, err := k.store.ListUsers()
	if err != nil {
		core.Logger.Error().Err(err).Str("component", "session").Msg("session: cannot list stored users")
		return k.cfg.RetryInterval
	}

	next := k.cfg.CheckInterval
	for _, user := range users {
		if ctx.Err() != nil {
			return next
		}
		next = k.checkAndRecord(ctx, user, next)
	}

	if next < minDelay {
		next = minDelay
	}
	return next
}

// checkAndRecord checks one user, stores the status and returns next,
// shortened to when this user needs its next check.
func (k *Keeper) checkAndRecord(ctx context.Context, user storage.UserSession, next time.Duration) time.Duration {
	status := k.checkUser(ctx, user)
	k.setStatus(status)

	// A failed re-login is retried sooner (but not in a tight loop).
	if status.Error != "" && status.CredentialsConfigured && k.cfg.RetryInterval < next {
		next = k.cfg.RetryInterval
	}
	// A check that could not reach a verdict is retried soon.
	if status.CheckError != "" && k.cfg.CheckErrorRetryInterval < next {
		next = k.cfg.CheckErrorRetryInterval
	}
	// Wake up in time to renew when the margin is reached before the
	// next regular check.
	if !status.ExpiresAt.IsZero() {
		untilRenew := status.ExpiresAt.Add(-k.cfg.RenewMargin).Sub(k.cfg.Now())
		if untilRenew > 0 && untilRenew < next {
			next = untilRenew
		}
	}
	return next
}

func (k *Keeper) checkUser(ctx context.Context, user storage.UserSession) Status {
	now := k.cfg.Now()
	status := Status{
		UserKey:               user.UserKey,
		Region:                user.Region,
		CheckedAt:             now.UTC(),
		LastRefresh:           user.LastRefresh.UTC(),
		ExpiresAt:             ExpiresAt(user, k.cfg.SessionLifetime).UTC(),
		CredentialsConfigured: k.cfg.Credentials.Matches(user.Region, user.Email),
	}
	previous, hasPrevious := k.getStatus(user.UserKey)
	if hasPrevious {
		status.LastReloginAt = previous.LastReloginAt
	}

	var validationErr error
	if user.SessionData == nil {
		validationErr = errors.New("no session data stored")
	} else if k.validate != nil {
		validationErr = k.validate(ctx, user.SessionData)
	}

	log := core.Logger.With().
		Str("component", "session").
		Str("user", user.UserKey).
		Str("region", user.Region).
		Str("expires_at", status.ExpiresAt.Format(time.RFC3339)).
		Logger()

	if validationErr != nil && IsTransient(validationErr) {
		// The check failed, not the session: it is no verdict.
		checkErr := validationErr
		status.CheckError = checkErr.Error()
		validationErr = nil
		if Decide(now, status.ExpiresAt, k.cfg.RenewMargin, nil) == DecisionKeep {
			// Keep the last verdict (before the first one: valid, as the
			// session is not expired), do not re-login, retry soon.
			status.Valid = true
			if hasPrevious {
				status.Valid = previous.Valid
				status.Error = previous.Error
			}
			log.Warn().Err(checkErr).Bool("valid", status.Valid).Dur("retry_in", k.cfg.CheckErrorRetryInterval).
				Msg("session: validation check failed (transport/server error), keeping previous validity")
			return status
		}
		// Expiring or expired by the clock: that decision does not depend
		// on the failed check, so renew (or report) as usual.
		log.Warn().Err(checkErr).Msg("session: validation check failed (transport/server error); session expiring by the clock")
	}

	decision := Decide(now, status.ExpiresAt, k.cfg.RenewMargin, validationErr)

	if decision == DecisionKeep {
		status.Valid = true
		log.Info().Msg("session: valid")
		return status
	}

	status.Valid = decision == DecisionRenewExpiring
	reason := "expiring"
	if decision == DecisionRenewInvalid {
		reason = "invalid"
		if validationErr != nil {
			status.Error = validationErr.Error()
		} else {
			status.Error = "session expired"
		}
	}

	if !status.CredentialsConfigured {
		event := log.Error()
		if validationErr != nil {
			event = event.AnErr("validation_error", validationErr)
		}
		event.Msgf("session: %s, no credentials configured (set %s/%s or --credentials-file, or run 'auth refresh')",
			reason, EnvEmail, EnvPassword)
		if status.Error == "" {
			status.Error = "session expiring and no credentials configured"
		}
		return status
	}

	log.Warn().Str("reason", reason).Msgf("session: %s, re-login", reason)

	sessionData, err := k.relogin(ctx, user)
	if err != nil {
		log.Error().Err(err).Str("reason", reason).Msg("session: re-login failed")
		status.Error = "re-login failed: " + err.Error()
		status.reloginFailed = true
		return status
	}

	status.Valid = true
	status.Error = ""
	status.CheckError = "" // the new session was validated
	status.LastReloginAt = k.cfg.Now().UTC()
	status.LastRefresh = status.LastReloginAt
	status.ExpiresAt = ExpiresAt(storage.UserSession{SessionData: sessionData, LastRefresh: k.cfg.Now()}, k.cfg.SessionLifetime).UTC()
	log.Info().Str("new_expires_at", status.ExpiresAt.Format(time.RFC3339)).Msg("session: re-login succeeded")
	return status
}

func (k *Keeper) relogin(ctx context.Context, user storage.UserSession) (*tuya.SessionData, error) {
	if k.login == nil {
		return nil, errors.New("no login function configured")
	}
	sessionData, err := k.login(ctx, user.Region, user.Email, k.cfg.Credentials.Password)
	if err != nil {
		return nil, err
	}
	if sessionData == nil {
		return nil, errors.New("login returned no session")
	}
	if k.validate != nil {
		if err := k.validate(ctx, sessionData); err != nil {
			return nil, errors.New("new session rejected: " + err.Error())
		}
	}
	// Save under the stored region/email so the same user file is replaced.
	if err := k.store.SaveUser(user.Region, user.Email, sessionData); err != nil {
		return nil, errors.New("saving new session: " + err.Error())
	}
	return sessionData, nil
}

func (k *Keeper) setStatus(status Status) {
	k.mutex.Lock()
	defer k.mutex.Unlock()
	k.statuses[status.UserKey] = status
}

func (k *Keeper) getStatus(userKey string) (Status, bool) {
	k.mutex.RLock()
	defer k.mutex.RUnlock()
	status, ok := k.statuses[userKey]
	return status, ok
}

// Statuses returns the last known status of every checked session, sorted by
// user key.
func (k *Keeper) Statuses() []Status {
	k.mutex.RLock()
	defer k.mutex.RUnlock()
	result := make([]Status, 0, len(k.statuses))
	for _, status := range k.statuses {
		result = append(result, status)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UserKey < result[j].UserKey })
	return result
}
