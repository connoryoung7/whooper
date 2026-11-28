package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"whooper/internal/syncer"
	"whooper/internal/whoop"
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /auth/login", s.requireAdmin(s.handleLogin))
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("GET /status", s.requireAdmin(s.handleStatus))
	return mux
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || s.opts.AdminPassword == "" ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.opts.AdminPassword)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="whooper", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, "whooper: WHOOP data mirror\n\n"+
		"  /auth/login   connect (or reconnect) your WHOOP account\n"+
		"  /status       is the full history downloaded? (JSON)\n"+
		"  /health       health check\n")
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	stats := s.opts.DB.Health(r.Context())
	status := http.StatusOK
	if stats["status"] != "up" {
		status = http.StatusServiceUnavailable
	}
	// /health is public, and driver errors name the database user, host and address.
	// Log the reason here and send clients only the status.
	if msg, ok := stats["error"]; ok {
		s.logf("health: database down: %s", msg)
		delete(stats, "error")
	}
	writeJSON(w, status, stats)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomState()
	if err == nil {
		err = s.opts.DB.CreateOAuthState(r.Context(), state)
	}
	if err != nil {
		s.logf("auth/login: %v", err)
		http.Error(w, "could not start authorization", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, s.opts.OAuth.AuthCodeURL(state), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, fmt.Sprintf("WHOOP authorization failed: %s %s", e, q.Get("error_description")), http.StatusBadRequest)
		return
	}
	// The code is single-use: finish even if the browser goes away mid-request.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
	defer cancel()

	ok, err := s.opts.DB.ConsumeOAuthState(ctx, q.Get("state"))
	if err != nil {
		s.logf("auth/callback: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "invalid or expired state; start again at /auth/login", http.StatusBadRequest)
		return
	}
	tok, err := s.opts.OAuth.Exchange(ctx, q.Get("code"))
	if err != nil {
		s.logf("auth/callback: exchange: %v", err)
		http.Error(w, "could not exchange the authorization code with WHOOP", http.StatusBadGateway)
		return
	}
	raw, err := s.opts.API.WithTokens(whoop.StaticToken(tok.AccessToken)).GetRaw(ctx, "/v2/user/profile/basic")
	if err != nil {
		s.logf("auth/callback: profile: %v", err)
		http.Error(w, "connected, but could not read the WHOOP profile", http.StatusBadGateway)
		return
	}
	var profile struct {
		UserID    int64  `json:"user_id"`
		FirstName string `json:"first_name"`
	}
	if err := json.Unmarshal(raw, &profile); err != nil || profile.UserID == 0 {
		http.Error(w, "unexpected WHOOP profile response", http.StatusBadGateway)
		return
	}
	// One deployment mirrors one account; don't mix someone else's data into it.
	existing, found, err := s.opts.DB.ProfileUserID(ctx)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if found && existing != profile.UserID {
		http.Error(w, fmt.Sprintf("this database holds data for WHOOP user %d; refusing to connect user %d", existing, profile.UserID), http.StatusConflict)
		return
	}
	if err := s.opts.DB.SaveToken(ctx, tok); err != nil {
		s.logf("auth/callback: saving token: %v", err)
		http.Error(w, "could not save the token", http.StatusInternalServerError)
		return
	}
	if _, err := s.opts.DB.UpsertProfile(ctx, raw, time.Now()); err != nil {
		s.logf("auth/callback: saving profile: %v", err)
	}
	s.startSync()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "Connected WHOOP user %d (%s).\n\n"+
		"A sync has started in the background. The first one downloads your entire history,\n"+
		"which takes a few minutes because of WHOOP's rate limit. Check /status for progress.\n",
		profile.UserID, profile.FirstName)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	rep, err := syncer.BuildReport(r.Context(), s.opts.DB, s.opts.StatusOptions)
	if err != nil {
		s.logf("status: %v", err)
		http.Error(w, "could not build status report", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// startSync runs a sync in the background (full if the history isn't downloaded yet).
func (s *Server) startSync() {
	s.wg.Go(func() {
		err := s.opts.Syncer.Run(s.opts.BaseContext, false)
		switch {
		case errors.Is(err, syncer.ErrLocked):
			s.logf("background sync skipped: %v", err)
		case err != nil:
			s.logf("background sync: %v", err)
		}
	})
}

// randomState returns 8 alphanumeric characters (WHOOP requires an 8-character state).
func randomState() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
