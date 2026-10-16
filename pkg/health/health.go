// Package health serves a small JSON health document for a supervising
// sidecar (CVS patch). It is meant to be bound to loopback only.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tuya-ipc-terminal/pkg/core"
	"tuya-ipc-terminal/pkg/rtsp"
	"tuya-ipc-terminal/pkg/session"
)

// StreamSource is implemented by *rtsp.RTSPServer.
type StreamSource interface {
	IsRunning() bool
	ListenAddr() string
	MediaTimeout() time.Duration
	StreamsHealth() []rtsp.StreamHealth
}

// SessionSource is implemented by *session.Keeper.
type SessionSource interface {
	Statuses() []session.Status
}

// Document is the /healthz response body. Times are UTC RFC3339; null
// means "never".
type Document struct {
	Status              string       `json:"status"` // ok | degraded | down
	Version             string       `json:"version"`
	GeneratedAt         string       `json:"generated_at"`
	RTSPListen          string       `json:"rtsp_listen"`
	RTSPRunning         bool         `json:"rtsp_running"`
	MediaTimeoutSeconds float64      `json:"media_timeout_seconds"`
	SessionKeeper       bool         `json:"session_keeper_enabled"`
	Streams             []StreamDoc  `json:"streams"`
	Sessions            []SessionDoc `json:"sessions"`
}

type StreamDoc struct {
	Device          string   `json:"device"`
	DeviceID        string   `json:"device_id"`
	RTSPPath        string   `json:"rtsp_path"`
	Resolution      string   `json:"resolution"`
	State           string   `json:"state"`
	Clients         int      `json:"clients"`
	StartedAt       *string  `json:"started_at"`
	LastMediaAt     *string  `json:"last_media_at"`
	LastAudioAt     *string  `json:"last_audio_at"`
	MediaAgeSeconds *float64 `json:"media_age_seconds"`
}

type SessionDoc struct {
	UserKey               string  `json:"user_key"`
	Region                string  `json:"region"`
	Valid                 bool    `json:"valid"`
	CheckedAt             *string `json:"checked_at"`
	ExpiresAt             *string `json:"expires_at"`
	LastRefresh           *string `json:"last_refresh"`
	LastReloginAt         *string `json:"last_relogin_at"`
	CredentialsConfigured bool    `json:"credentials_configured"`
	Error                 string  `json:"error,omitempty"`
	// CheckError: the last validation failed for transport/server reasons;
	// valid/error carry the previous result. Does not degrade the status.
	CheckError string `json:"check_error,omitempty"`
}

func timeString(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// Build assembles the health document. sessions may be nil (keeper disabled).
func Build(version string, streams StreamSource, sessions SessionSource, now time.Time) Document {
	doc := Document{
		Status:      "ok",
		Version:     version,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Streams:     []StreamDoc{},
		Sessions:    []SessionDoc{},
	}

	if streams != nil {
		doc.RTSPListen = streams.ListenAddr()
		doc.RTSPRunning = streams.IsRunning()
		doc.MediaTimeoutSeconds = streams.MediaTimeout().Seconds()
		for _, stream := range streams.StreamsHealth() {
			entry := StreamDoc{
				Device:      stream.Device,
				DeviceID:    stream.DeviceID,
				RTSPPath:    stream.RTSPPath,
				Resolution:  stream.Resolution,
				State:       stream.State,
				Clients:     stream.Clients,
				StartedAt:   timeString(stream.StartedAt),
				LastMediaAt: timeString(stream.LastMediaAt),
				LastAudioAt: timeString(stream.LastAudioAt),
			}
			if !stream.LastMediaAt.IsZero() {
				age := now.Sub(stream.LastMediaAt).Seconds()
				entry.MediaAgeSeconds = &age
			}
			doc.Streams = append(doc.Streams, entry)
		}
	}
	if streams == nil || !doc.RTSPRunning {
		doc.Status = "down"
	}

	if sessions != nil {
		doc.SessionKeeper = true
		for _, status := range sessions.Statuses() {
			doc.Sessions = append(doc.Sessions, SessionDoc{
				UserKey:               status.UserKey,
				Region:                status.Region,
				Valid:                 status.Valid,
				CheckedAt:             timeString(status.CheckedAt),
				ExpiresAt:             timeString(status.ExpiresAt),
				LastRefresh:           timeString(status.LastRefresh),
				LastReloginAt:         timeString(status.LastReloginAt),
				CredentialsConfigured: status.CredentialsConfigured,
				Error:                 status.Error,
				CheckError:            status.CheckError,
			})
			if (!status.Valid || status.Error != "") && doc.Status == "ok" {
				doc.Status = "degraded"
			}
		}
	}

	return doc
}

// Handler serves GET /healthz.
func Handler(version string, streams StreamSource, sessions SessionSource) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		doc := Build(version, streams, sessions, time.Now())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if doc.Status == "down" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	return mux
}

// NormalizeAddr turns "8556" or ":8556" into "127.0.0.1:8556" so the health
// endpoint binds to loopback unless a host is given explicitly.
func NormalizeAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("empty address")
	}
	if _, err := strconv.Atoi(addr); err == nil {
		addr = ":" + addr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if host == "" {
		host = rtsp.DefaultListenAddress
	}
	return net.JoinHostPort(host, port), nil
}

// Serve starts the health HTTP server and returns once it is listening. It
// shuts down when ctx is cancelled.
func Serve(ctx context.Context, addr string, handler http.Handler) (string, error) {
	listenAddr, err := NormalizeAddr(addr)
	if err != nil {
		return "", err
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", err
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			core.Logger.Error().Err(err).Str("component", "health").Msg("health: server stopped")
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	return listener.Addr().String(), nil
}
