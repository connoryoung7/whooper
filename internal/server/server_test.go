package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"whooper/internal/database"
	"whooper/internal/syncer"
	"whooper/internal/testpg"
	"whooper/internal/whoop"
	"whooper/internal/whoop/whooptest"
)

var pg *testpg.Server

func TestMain(m *testing.M) {
	var err error
	pg, err = testpg.Start(context.Background())
	if err != nil {
		log.Fatalf("could not start postgres container: %v", err)
	}
	code := m.Run()
	pg.Close()
	os.Exit(code)
}

const password = "hunter2"

type env struct {
	db   *database.DB
	fake *whooptest.Server
	srv  *Server
	http *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := database.Open(context.Background(), pg.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	fake := whooptest.New()
	fake.Seed(3, 1)
	oauth := fake.OAuth()
	api := fake.Client(&whoop.TokenSource{OAuth: oauth, Store: db})
	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Options{
		DB: db, OAuth: oauth, API: api, AdminPassword: password, BaseContext: ctx,
		Syncer: &syncer.Syncer{DB: db, API: api, Lookback: 24 * time.Hour, Logf: log.Printf},
		Logf:   log.Printf,
	})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		hs.Close()
		cancel()
		srv.Wait()
		fake.Close()
		db.Close()
	})
	return &env{db: db, fake: fake, srv: srv, http: hs}
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *env) get(t *testing.T, path string, admin bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.http.URL+path, nil)
	if admin {
		req.SetBasicAuth("admin", password)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// login starts the OAuth flow and returns the state WHOOP would echo back.
func (e *env) login(t *testing.T) string {
	t.Helper()
	resp := e.get(t, "/auth/login", true)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loc.String(), e.fake.URL+"/oauth/oauth2/auth?") {
		t.Fatalf("redirected to %s", loc)
	}
	state := loc.Query().Get("state")
	if len(state) != 8 {
		t.Fatalf("state %q is not 8 characters", state)
	}
	return state
}

func TestAdminRoutesRequirePassword(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/status", "/auth/login"} {
		if got := e.get(t, p, false).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("%s without password: %d, want 401", p, got)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, e.http.URL+"/status", nil)
	req.SetBasicAuth("admin", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password: %d, want 401", resp.StatusCode)
	}
}

func TestHealth(t *testing.T) {
	e := newEnv(t)
	if got := e.get(t, "/health", false).StatusCode; got != http.StatusOK {
		t.Fatalf("health = %d", got)
	}
}

func TestConnectFlowStoresTokenAndStartsBackfill(t *testing.T) {
	e := newEnv(t)
	state := e.login(t)
	e.fake.AddCode("the-code")

	resp := e.get(t, "/auth/callback?code=the-code&state="+state, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", resp.StatusCode)
	}
	e.srv.Wait() // the background sync started by the callback

	var rep syncer.Report
	resp = e.get(t, "/status", true)
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Complete || rep.WhoopUserID != 4242 || !rep.Token.Connected {
		t.Fatalf("report after connect: complete=%v user=%d problems=%v", rep.Complete, rep.WhoopUserID, rep.Problems)
	}
}

func TestCallbackRejectsUnknownState(t *testing.T) {
	e := newEnv(t)
	e.fake.AddCode("the-code")
	if got := e.get(t, "/auth/callback?code=the-code&state=forged12", false).StatusCode; got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got)
	}
	if info, _ := e.db.TokenInfo(context.Background()); info.Connected {
		t.Fatal("token stored despite a bad state")
	}
}

func TestCallbackRefusesADifferentWhoopUser(t *testing.T) {
	e := newEnv(t)
	if _, err := e.db.UpsertProfile(context.Background(), json.RawMessage(`{"user_id":999}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	state := e.login(t)
	e.fake.AddCode("the-code")
	if got := e.get(t, "/auth/callback?code=the-code&state="+state, false).StatusCode; got != http.StatusConflict {
		t.Fatalf("status = %d, want 409", got)
	}
	if info, _ := e.db.TokenInfo(context.Background()); info.Connected {
		t.Fatal("token for a different user was stored")
	}
}
