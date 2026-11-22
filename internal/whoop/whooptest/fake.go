// Package whooptest provides an in-memory fake of the WHOOP API for tests.
package whooptest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"whooper/internal/whoop"
)

const (
	ClientID     = "test-client"
	ClientSecret = "test-secret"
	RedirectURL  = "https://whooper.test/auth/callback"
)

// Collection paths as served by the fake (relative to the API base, which ends in /developer).
const (
	CyclePath    = "/v2/cycle"
	SleepPath    = "/v2/activity/sleep"
	RecoveryPath = "/v2/recovery"
	WorkoutPath  = "/v2/activity/workout"
)

type record struct {
	key  string
	at   time.Time // sort and filter time: start, or the related sleep's start for recoveries
	data map[string]any
}

type day struct {
	cycleID int64
	sleepID string
	start   time.Time
}

// Server is a fake WHOOP API: OAuth token endpoint plus profile, body and collection endpoints
// with WHOOP-style pagination (limit ≤ 25, nextToken, start filter, newest first).
type Server struct {
	*httptest.Server

	mu        sync.Mutex
	userID    int64
	data      map[string][]record
	days      []day
	nextStart time.Time
	workouts  int
	access    map[string]bool
	refresh   map[string]bool
	codes     map[string]bool
	seq       int
	refreshes int
	queries   map[string][]url.Values
}

func New() *Server {
	s := &Server{
		userID:  4242,
		data:    map[string][]record{},
		access:  map[string]bool{},
		refresh: map[string]bool{},
		codes:   map[string]bool{},
		queries: map[string][]url.Values{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/oauth2/token", s.handleToken)
	mux.HandleFunc("GET /developer/v2/user/profile/basic", s.authed(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		id := s.userID
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"user_id": id, "email": "test@example.com", "first_name": "Test", "last_name": "User"})
	}))
	mux.HandleFunc("GET /developer/v2/user/measurement/body", s.authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"height_meter": 1.8, "weight_kilogram": 80.5, "max_heart_rate": 190})
	}))
	for _, p := range []string{CyclePath, SleepPath, RecoveryPath, WorkoutPath} {
		mux.HandleFunc("GET /developer"+p, s.authed(s.collection(p)))
	}
	s.Server = httptest.NewServer(mux)
	return s
}

// OAuth returns an OAuth config pointed at the fake.
func (s *Server) OAuth() *whoop.OAuth {
	o := whoop.NewOAuth(ClientID, ClientSecret, RedirectURL)
	o.AuthURL = s.URL + "/oauth/oauth2/auth"
	o.TokenURL = s.URL + "/oauth/oauth2/token"
	return o
}

// Client returns an API client pointed at the fake with rate limiting and backoff disabled.
func (s *Server) Client(tokens whoop.AccessTokener) *whoop.Client {
	c := whoop.NewClient(tokens)
	c.BaseURL = s.URL + "/developer"
	c.MinInterval = 0
	c.RetryBase = time.Millisecond
	c.Logf = func(string, ...any) {}
	return c
}

// SetUserID changes the WHOOP user the fake reports.
func (s *Server) SetUserID(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userID = id
}

// Issue mints a valid token pair, as if the user had authorized.
func (s *Server) Issue() *whoop.Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issueLocked()
}

func (s *Server) issueLocked() *whoop.Token {
	s.seq++
	t := &whoop.Token{
		AccessToken:  fmt.Sprintf("access-%d", s.seq),
		RefreshToken: fmt.Sprintf("refresh-%d", s.seq),
		ExpiresAt:    time.Now().Add(time.Hour),
		Scope:        strings.Join(whoop.Scopes, " "),
	}
	s.access[t.AccessToken] = true
	s.refresh[t.RefreshToken] = true
	return t
}

// RevokeAccessTokens invalidates every issued access token (refresh tokens still work).
func (s *Server) RevokeAccessTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.access)
}

// AddCode registers an authorization code that the token endpoint will accept once.
func (s *Server) AddCode(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code] = true
}

// Refreshes reports how many refresh_token grants succeeded.
func (s *Server) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// Queries returns the query strings of every request made to a collection path.
func (s *Server) Queries(path string) []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.queries[path]...)
}

// Seed adds `days` consecutive physiological days (a cycle, sleep and recovery each; cycles
// are contiguous and the newest is still open) ending around now, plus `workouts` workouts.
func (s *Server) Seed(days, workouts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextStart.IsZero() {
		s.nextStart = time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(days) * 24 * time.Hour)
	}
	for range days {
		s.addDayLocked()
	}
	for range workouts {
		s.addWorkoutLocked()
	}
}

// AddDay adds one more day after the newest one.
func (s *Server) AddDay() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addDayLocked()
}

func (s *Server) addDayLocked() {
	n := len(s.days)
	start := s.nextStart
	s.nextStart = start.Add(24 * time.Hour)
	d := day{cycleID: int64(1000 + n), sleepID: fmt.Sprintf("00000000-0000-4000-8000-%012d", n), start: start}
	if n > 0 {
		prev := s.days[n-1]
		for _, r := range s.data[CyclePath] {
			if r.key == strconv.FormatInt(prev.cycleID, 10) {
				r.data["end"] = ts(start)
			}
		}
	}
	s.days = append(s.days, d)
	s.insertLocked(CyclePath, record{key: strconv.FormatInt(d.cycleID, 10), at: start, data: map[string]any{
		"id": d.cycleID, "user_id": s.userID, "created_at": ts(start), "updated_at": ts(start),
		"start": ts(start), "end": nil, "timezone_offset": "-05:00", "score_state": "SCORED",
		"score": map[string]any{"strain": 10.5, "kilojoule": 8000.0, "average_heart_rate": 65, "max_heart_rate": 160},
	}})
	s.insertLocked(SleepPath, record{key: d.sleepID, at: start, data: map[string]any{
		"id": d.sleepID, "cycle_id": d.cycleID, "v1_id": n, "user_id": s.userID,
		"created_at": ts(start), "updated_at": ts(start), "start": ts(start), "end": ts(start.Add(8 * time.Hour)),
		"timezone_offset": "-05:00", "nap": false, "score_state": "SCORED",
		"score": map[string]any{"sleep_performance_percentage": 90.0, "respiratory_rate": 15.2},
	}})
	s.insertLocked(RecoveryPath, record{key: strconv.FormatInt(d.cycleID, 10), at: start, data: map[string]any{
		"cycle_id": d.cycleID, "sleep_id": d.sleepID, "user_id": s.userID,
		"created_at": ts(start.Add(8 * time.Hour)), "updated_at": ts(start.Add(8 * time.Hour)), "score_state": "SCORED",
		"score": map[string]any{"user_calibrating": false, "recovery_score": 70.0, "resting_heart_rate": 50.0, "hrv_rmssd_milli": 60.5},
	}})
}

func (s *Server) addWorkoutLocked() {
	if len(s.days) == 0 {
		panic("whooptest: seed days before workouts")
	}
	i := s.workouts
	s.workouts++
	d := s.days[len(s.days)-1-i%len(s.days)]
	start := d.start.Add(12*time.Hour + time.Duration(i/len(s.days))*time.Hour)
	id := fmt.Sprintf("00000000-0000-4000-9000-%012d", i)
	s.insertLocked(WorkoutPath, record{key: id, at: start, data: map[string]any{
		"id": id, "v1_id": i, "user_id": s.userID, "created_at": ts(start), "updated_at": ts(start),
		"start": ts(start), "end": ts(start.Add(time.Hour)), "timezone_offset": "-05:00",
		"sport_name": "running", "sport_id": 0, "score_state": "SCORED",
		"score": map[string]any{"strain": 8.2, "average_heart_rate": 140},
	}})
}

func (s *Server) insertLocked(path string, r record) {
	recs := append(s.data[path], r)
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].at.After(recs[j].at) })
	s.data[path] = recs
}

// Keys lists the record keys in a collection, newest first. Cycles and recoveries are keyed
// by cycle id, sleeps and workouts by UUID.
func (s *Server) Keys(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for _, r := range s.data[path] {
		keys = append(keys, r.key)
	}
	return keys
}

// Remove deletes one record from a collection.
func (s *Server) Remove(path, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.data[path][:0]
	for _, r := range s.data[path] {
		if r.key != key {
			recs = append(recs, r)
		}
	}
	s.data[path] = recs
}

// RemoveDay deletes the cycle, sleep and recovery of the i-th seeded day, leaving a gap.
func (s *Server) RemoveDay(i int) {
	s.mu.Lock()
	d := s.days[i]
	s.mu.Unlock()
	cycle := strconv.FormatInt(d.cycleID, 10)
	s.Remove(CyclePath, cycle)
	s.Remove(RecoveryPath, cycle)
	s.Remove(SleepPath, d.sleepID)
}

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		valid := ok && s.access[token]
		s.mu.Unlock()
		if !valid {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) collection(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.mu.Lock()
		s.queries[path] = append(s.queries[path], q)
		s.mu.Unlock()

		limit := 10
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 25 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit must be 1-25"})
				return
			}
			limit = n
		}
		var start time.Time
		if v := q.Get("start"); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad start"})
				return
			}
			start = t
		}
		offset := 0
		if v := q.Get("nextToken"); v != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(v, "page-"))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad nextToken"})
				return
			}
			offset = n
		}

		s.mu.Lock()
		var matched []map[string]any
		for _, rec := range s.data[path] {
			if start.IsZero() || !rec.at.Before(start) {
				matched = append(matched, rec.data)
			}
		}
		page := []map[string]any{}
		end := min(offset+limit, len(matched))
		if offset < end {
			page = matched[offset:end]
		}
		resp := map[string]any{"records": page, "next_token": nil}
		if end < len(matched) {
			resp["next_token"] = fmt.Sprintf("page-%d", end)
		}
		body, err := json.Marshal(resp)
		s.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	f := r.PostForm
	if f.Get("client_id") != ClientID || f.Get("client_secret") != ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch f.Get("grant_type") {
	case "authorization_code":
		if !s.codes[f.Get("code")] || f.Get("redirect_uri") != RedirectURL {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		delete(s.codes, f.Get("code"))
	case "refresh_token":
		if !s.refresh[f.Get("refresh_token")] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "refresh token is invalid"})
			return
		}
		delete(s.refresh, f.Get("refresh_token")) // rotation: the old refresh token is spent
		s.refreshes++
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	t := s.issueLocked()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": t.AccessToken, "refresh_token": t.RefreshToken,
		"expires_in": 3600, "scope": t.Scope, "token_type": "bearer",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
