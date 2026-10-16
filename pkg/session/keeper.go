// Package session keeps stored Tuya web sessions alive (CVS patch).
//
// The bridge authenticates once (QR code or email/password) and stores the
// session cookies under .tuya-data. The cookies expire after roughly four
// days and nothing in upstream renews them, so streams silently stop
// starting. The Keeper periodically validates every stored session and, when
// it is invalid or close to its (stored or assumed) expiry, re-logs in
// non-interactively with operator-supplied credentials.
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
	minDelay               = time.Minute
)

// Store is the subset of storage.StorageManager used by the keeper.
type Store interface {
	ListUsers() ([]storage.UserSession, error)
	SaveUser(region, email string, sessionData *tuya.SessionData) error
}

// ValidateFunc returns nil when the stored session is accepted by Tuya.
type ValidateFunc func(ctx context.Context, sessionData *tuya.SessionData) error

// LoginFunc performs a non-interactive email/password login.
type LoginFunc func(ctx context.Context, region, email, password string) (*tuya.SessionData, error)

// Config configures a Keeper.
type Config struct {
	CheckInterval   time.Duration // how often sessions are validated
	RenewMargin     time.Duration // re-login this long before expiry
	SessionLifetime time.Duration // assumed lifetime when cookies carry no expiry
	RetryInterval   time.Duration // next check after a failed re-login
	Credentials     *Credentials  // nil: cannot re-login, only report
	Now             func() time.Time
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
}

// Keeper validates and renews stored sessions.
type Keeper struct {
	cfg      Config
	store    Store
	validate ValidateFunc
	login    LoginFunc

	mutex    sync.RWMutex
	statuses map[string]Status
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
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Keeper{
		cfg:      cfg,
		store:    store,
		validate: validate,
		login:    login,
		statuses: make(map[string]Status),
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
func (k *Keeper) Run(ctx context.Context) {
	for {
		delay := k.CheckAll(ctx)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
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
		status := k.checkUser(ctx, user)
		k.setStatus(status)

		// A failed re-login is retried sooner (but not in a tight loop).
		if status.Error != "" && status.CredentialsConfigured && k.cfg.RetryInterval < next {
			next = k.cfg.RetryInterval
		}
		// Wake up in time to renew when the margin is reached before the
		// next regular check.
		if !status.ExpiresAt.IsZero() {
			untilRenew := status.ExpiresAt.Add(-k.cfg.RenewMargin).Sub(k.cfg.Now())
			if untilRenew > 0 && untilRenew < next {
				next = untilRenew
			}
		}
	}

	if next < minDelay {
		next = minDelay
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
	if previous, ok := k.getStatus(user.UserKey); ok {
		status.LastReloginAt = previous.LastReloginAt
	}

	var validationErr error
	if user.SessionData == nil {
		validationErr = errors.New("no session data stored")
	} else if k.validate != nil {
		validationErr = k.validate(ctx, user.SessionData)
	}

	decision := Decide(now, status.ExpiresAt, k.cfg.RenewMargin, validationErr)
	log := core.Logger.With().
		Str("component", "session").
		Str("user", user.UserKey).
		Str("region", user.Region).
		Str("expires_at", status.ExpiresAt.Format(time.RFC3339)).
		Logger()

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
		return status
	}

	status.Valid = true
	status.Error = ""
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
