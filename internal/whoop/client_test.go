package whoop_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"whooper/internal/whoop"
	"whooper/internal/whoop/whooptest"
)

type memStore struct {
	mu  sync.Mutex
	tok *whoop.Token
}

func (m *memStore) UpdateToken(_ context.Context, fn func(*whoop.Token) (*whoop.Token, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cur *whoop.Token
	if m.tok != nil {
		c := *m.tok
		cur = &c
	}
	next, err := fn(cur)
	if err != nil {
		return err
	}
	if next != nil {
		m.tok = next
	}
	return nil
}

func TestForEachPageWalksWholeHistory(t *testing.T) {
	fake := whooptest.New()
	defer fake.Close()
	fake.Seed(60, 0)
	c := fake.Client(whoop.StaticToken(fake.Issue().AccessToken))

	var ids []int64
	pages := 0
	err := c.ForEachPage(context.Background(), whooptest.CyclePath, time.Time{}, func(recs []json.RawMessage) error {
		pages++
		for _, r := range recs {
			var v struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(r, &v); err != nil {
				return err
			}
			ids = append(ids, v.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pages != 3 || len(ids) != 60 {
		t.Fatalf("got %d records over %d pages, want 60 over 3", len(ids), pages)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] >= ids[i-1] {
			t.Fatalf("records not newest-first or duplicated at %d: %v", i, ids[i-1:i+1])
		}
	}
	qs := fake.Queries(whooptest.CyclePath)
	if qs[0].Get("limit") != "25" || qs[0].Has("start") || qs[0].Has("nextToken") {
		t.Errorf("first request query = %v; want limit=25 and no start/nextToken", qs[0])
	}
	if qs[1].Get("nextToken") == "" {
		t.Errorf("second request did not pass nextToken: %v", qs[1])
	}
}

func TestForEachPageStartFilter(t *testing.T) {
	fake := whooptest.New()
	defer fake.Close()
	fake.Seed(30, 0)
	c := fake.Client(whoop.StaticToken(fake.Issue().AccessToken))

	since := time.Now().Add(-5*24*time.Hour - time.Hour)
	n := 0
	err := c.ForEachPage(context.Background(), whooptest.CyclePath, since, func(recs []json.RawMessage) error {
		n += len(recs)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("got %d cycles since %s, want 5", n, since)
	}
	if got := fake.Queries(whooptest.CyclePath)[0].Get("start"); got != whoop.FormatTime(since) {
		t.Fatalf("start = %q, want %q", got, whoop.FormatTime(since))
	}
}

func TestForEachPageRejectsRepeatedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"records":[{"id":1}],"next_token":"same"}`))
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	err := c.ForEachPage(context.Background(), "/v2/cycle", time.Time{}, func([]json.RawMessage) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("err = %v, want repeated next_token error", err)
	}
}

func TestClientRetriesRateLimitAndServerErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("X-RateLimit-Reset", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.Write([]byte(`{"user_id":1}`))
		}
	}))
	defer srv.Close()
	raw, err := testClient(t, srv.URL).GetRaw(context.Background(), "/v2/user/profile/basic")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"user_id":1}` || calls.Load() != 3 {
		t.Fatalf("raw=%s calls=%d", raw, calls.Load())
	}
}

func TestClientGivesUpOnLongRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Reset", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := testClient(t, srv.URL).GetRaw(context.Background(), "/v2/cycle")
	if !errors.Is(err, whoop.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (no point retrying for an hour)", calls.Load())
	}
}

func TestClientDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	_, err := testClient(t, srv.URL).GetRaw(context.Background(), "/v2/cycle")
	var apiErr *whoop.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("err = %v calls = %d", err, calls.Load())
	}
}

func TestClientRefreshesRejectedAccessToken(t *testing.T) {
	fake := whooptest.New()
	defer fake.Close()
	orig := fake.Issue()
	store := &memStore{tok: orig}
	fake.RevokeAccessTokens() // stored token looks valid but the API rejects it

	c := fake.Client(&whoop.TokenSource{OAuth: fake.OAuth(), Store: store})
	if _, err := c.GetRaw(context.Background(), "/v2/user/profile/basic"); err != nil {
		t.Fatal(err)
	}
	if fake.Refreshes() != 1 {
		t.Fatalf("refreshes = %d, want 1", fake.Refreshes())
	}
	if store.tok.RefreshToken == orig.RefreshToken {
		t.Fatal("rotated refresh token was not persisted")
	}
}

func TestTokenSourceRefreshesExpiredTokenOnce(t *testing.T) {
	fake := whooptest.New()
	defer fake.Close()
	tok := fake.Issue()
	tok.ExpiresAt = time.Now().Add(-time.Minute)
	store := &memStore{tok: tok}
	ts := &whoop.TokenSource{OAuth: fake.OAuth(), Store: store}

	a1, err := ts.AccessToken(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := ts.AccessToken(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if a1 != a2 || a1 == tok.AccessToken {
		t.Fatalf("a1=%s a2=%s orig=%s", a1, a2, tok.AccessToken)
	}
	if fake.Refreshes() != 1 {
		t.Fatalf("refreshes = %d, want 1", fake.Refreshes())
	}
	if store.tok.AccessToken != a1 || store.tok.RefreshToken == tok.RefreshToken {
		t.Fatalf("store not updated with the rotated pair: %+v", store.tok)
	}
}

func TestTokenSourceReportsRevokedRefreshToken(t *testing.T) {
	fake := whooptest.New()
	defer fake.Close()
	store := &memStore{tok: &whoop.Token{AccessToken: "x", RefreshToken: "spent", ExpiresAt: time.Now().Add(-time.Hour)}}
	_, err := (&whoop.TokenSource{OAuth: fake.OAuth(), Store: store}).AccessToken(context.Background(), "")
	if !errors.Is(err, whoop.ErrReauthorize) {
		t.Fatalf("err = %v, want ErrReauthorize", err)
	}
}

func TestTokenSourceWithoutToken(t *testing.T) {
	fake := whooptest.New()
	defer fake.Close()
	_, err := (&whoop.TokenSource{OAuth: fake.OAuth(), Store: &memStore{}}).AccessToken(context.Background(), "")
	if !errors.Is(err, whoop.ErrNotAuthorized) {
		t.Fatalf("err = %v, want ErrNotAuthorized", err)
	}
}

func TestAuthCodeURL(t *testing.T) {
	o := whoop.NewOAuth("id", "secret", "https://example.com/cb")
	u, err := url.Parse(o.AuthCodeURL("abcdefgh"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "id" || q.Get("state") != "abcdefgh" || q.Get("redirect_uri") != "https://example.com/cb" {
		t.Fatalf("query = %v", q)
	}
	if !strings.Contains(" "+q.Get("scope")+" ", " offline ") {
		t.Fatalf("scope %q lacks offline", q.Get("scope"))
	}
}

func testClient(t *testing.T, base string) *whoop.Client {
	c := whoop.NewClient(whoop.StaticToken("token"))
	c.BaseURL = base
	c.MinInterval = 0
	c.RetryBase = time.Millisecond
	c.Logf = t.Logf
	return c
}
