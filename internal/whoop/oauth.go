// Package whoop is a small client for the WHOOP Developer API (v2).
package whoop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	DefaultAPIBase  = "https://api.prod.whoop.com/developer"
	DefaultAuthURL  = "https://api.prod.whoop.com/oauth/oauth2/auth"
	DefaultTokenURL = "https://api.prod.whoop.com/oauth/oauth2/token"
)

// Scopes requested at authorization. "offline" is what makes WHOOP issue a refresh token,
// which is required for unattended (cron) syncs.
var Scopes = []string{
	"offline",
	"read:profile",
	"read:body_measurement",
	"read:cycles",
	"read:recovery",
	"read:sleep",
	"read:workout",
}

var (
	ErrNotAuthorized = errors.New("whoop: no account connected yet; open /auth/login on the server")
	ErrReauthorize   = errors.New("whoop: the stored refresh token was rejected; open /auth/login on the server to reconnect")
)

// Token is an OAuth token pair. WHOOP rotates refresh tokens: every refresh returns a new
// refresh token and the old one stops working, so the newest pair must always be persisted.
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scope        string
}

// OAuth performs the WHOOP OAuth 2.0 authorization-code and refresh flows.
type OAuth struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	AuthURL      string
	TokenURL     string
	HTTP         *http.Client
}

func NewOAuth(clientID, clientSecret, redirectURL string) *OAuth {
	return &OAuth{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		AuthURL:      DefaultAuthURL,
		TokenURL:     DefaultTokenURL,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
	}
}

// AuthCodeURL is where the user is sent to grant access. WHOOP requires state to be 8 characters.
func (o *OAuth) AuthCodeURL(state string) string {
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {o.ClientID},
		"redirect_uri":  {o.RedirectURL},
		"scope":         {strings.Join(Scopes, " ")},
		"state":         {state},
	}
	return o.AuthURL + "?" + q.Encode()
}

// Exchange trades an authorization code for a token pair.
func (o *OAuth) Exchange(ctx context.Context, code string) (*Token, error) {
	return o.tokenRequest(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {o.RedirectURL},
	})
}

// Refresh trades a refresh token for a new token pair. The old refresh token is spent.
func (o *OAuth) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	t, err := o.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"offline"},
	})
	var oe *oauthError
	if errors.As(err, &oe) && (oe.Code == "invalid_grant" || oe.Status == http.StatusUnauthorized) {
		return nil, fmt.Errorf("%w (%v)", ErrReauthorize, err)
	}
	return t, err
}

type oauthError struct {
	Status      int
	Code        string
	Description string
}

func (e *oauthError) Error() string {
	return fmt.Sprintf("whoop oauth: HTTP %d: %s %s", e.Status, e.Code, e.Description)
}

func (o *OAuth) tokenRequest(ctx context.Context, form url.Values) (*Token, error) {
	form.Set("client_id", o.ClientID)
	form.Set("client_secret", o.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("whoop oauth: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("whoop oauth: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		oe := &oauthError{Status: resp.StatusCode}
		var payload struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
			oe.Code, oe.Description = payload.Error, payload.Description
		} else {
			oe.Description = snippet(body)
		}
		return nil, oe
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("whoop oauth: decoding token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("whoop oauth: token response has no access_token")
	}
	if tr.RefreshToken == "" {
		return nil, errors.New("whoop oauth: token response has no refresh_token (was the offline scope granted?)")
	}
	expiresIn := time.Duration(tr.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = time.Hour
	}
	return &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(expiresIn),
		Scope:        tr.Scope,
	}, nil
}

// TokenStore persists the single token pair. UpdateToken must hold an exclusive lock across
// fn so that two processes never refresh at once (the loser's refresh token would already be
// spent, and WHOOP may revoke the whole grant on refresh-token reuse).
type TokenStore interface {
	// UpdateToken calls fn with the stored token (nil if none). If fn returns a non-nil token
	// it replaces the stored one before the lock is released.
	UpdateToken(ctx context.Context, fn func(current *Token) (*Token, error)) error
}

// AccessTokener supplies access tokens to the API client.
type AccessTokener interface {
	// AccessToken returns a usable access token. rejected, if non-empty, is a token the API
	// just refused with 401; it will not be returned again.
	AccessToken(ctx context.Context, rejected string) (string, error)
}

// expiryMargin refreshes slightly early so a token never expires mid-request.
const expiryMargin = 2 * time.Minute

// TokenSource hands out access tokens from a TokenStore, refreshing and persisting the
// rotated pair when the access token is about to expire or was rejected.
type TokenSource struct {
	OAuth *OAuth
	Store TokenStore

	mu     sync.Mutex
	cached *Token
}

func (s *TokenSource) AccessToken(ctx context.Context, rejected string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.cached; t != nil && t.AccessToken != rejected && fresh(t) {
		return t.AccessToken, nil
	}
	// Don't let a cancelled caller abort a refresh halfway: WHOOP may already have rotated the
	// refresh token, and losing the new one would force a manual reconnect.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var result *Token
	err := s.Store.UpdateToken(ctx, func(cur *Token) (*Token, error) {
		if cur == nil {
			return nil, ErrNotAuthorized
		}
		// Another process may have refreshed already; use its token if it's still good.
		if cur.AccessToken != rejected && fresh(cur) {
			result = cur
			return nil, nil
		}
		next, err := s.OAuth.Refresh(ctx, cur.RefreshToken)
		if err != nil {
			return nil, err
		}
		if next.Scope == "" {
			next.Scope = cur.Scope
		}
		result = next
		return next, nil
	})
	if err != nil {
		return "", err
	}
	s.cached = result
	return result.AccessToken, nil
}

func fresh(t *Token) bool { return time.Until(t.ExpiresAt) > expiryMargin }

// StaticToken is an AccessTokener for a token that can't be refreshed, such as one that was
// just issued by Exchange.
type StaticToken string

func (t StaticToken) AccessToken(_ context.Context, rejected string) (string, error) {
	if rejected != "" {
		return "", errors.New("whoop: access token was rejected")
	}
	return string(t), nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
