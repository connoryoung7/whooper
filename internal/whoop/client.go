package whoop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PageLimit is the largest page size WHOOP allows on collection endpoints.
const PageLimit = 25

// ErrRateLimited means WHOOP wants us to wait longer than Client.MaxWait (typically because
// the 10,000/day quota is spent). The next scheduled run will pick up where this one stopped.
var ErrRateLimited = errors.New("whoop: rate limit exhausted")

// APIError is a non-retryable HTTP error from the WHOOP API.
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("whoop: GET %s: HTTP %d: %s", e.Path, e.Status, e.Body)
}

// Client calls the WHOOP API with client-side rate limiting, retries for 429/5xx/network
// errors, and a token refresh on 401.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Tokens  AccessTokener

	// MinInterval spaces requests out. WHOOP allows 100 requests/minute.
	MinInterval time.Duration
	// MaxWait is the longest the client will pause for a rate limit before giving up.
	MaxWait time.Duration
	// MaxAttempts bounds retries per request.
	MaxAttempts int
	// RetryBase is the first backoff delay for 5xx and network errors; it doubles each retry.
	RetryBase time.Duration
	Logf      func(format string, args ...any)

	mu        sync.Mutex
	notBefore time.Time
	requests  atomic.Int64
}

func NewClient(tokens AccessTokener) *Client {
	return &Client{
		BaseURL:     DefaultAPIBase,
		HTTP:        &http.Client{Timeout: 60 * time.Second},
		Tokens:      tokens,
		MinInterval: 650 * time.Millisecond, // ~92 requests/minute
		MaxWait:     5 * time.Minute,
		MaxAttempts: 6,
		RetryBase:   2 * time.Second,
		Logf:        log.Printf,
	}
}

// WithTokens returns a client with the same settings but different credentials and its own
// rate limiter.
func (c *Client) WithTokens(t AccessTokener) *Client {
	n := NewClient(t)
	n.BaseURL, n.HTTP = c.BaseURL, c.HTTP
	n.MinInterval, n.MaxWait, n.MaxAttempts, n.RetryBase, n.Logf = c.MinInterval, c.MaxWait, c.MaxAttempts, c.RetryBase, c.Logf
	return n
}

// Requests reports how many HTTP requests this client has sent.
func (c *Client) Requests() int64 { return c.requests.Load() }

// Page is one page of a WHOOP collection endpoint.
type Page struct {
	Records   []json.RawMessage `json:"records"`
	NextToken string            `json:"next_token"`
}

// ListPage fetches one page of a collection such as /v2/cycle. A zero start applies no lower
// bound, which is how the whole history is requested.
func (c *Client) ListPage(ctx context.Context, path string, start time.Time, nextToken string) (*Page, error) {
	q := url.Values{"limit": {strconv.Itoa(PageLimit)}}
	if !start.IsZero() {
		q.Set("start", FormatTime(start))
	}
	if nextToken != "" {
		q.Set("nextToken", nextToken)
	}
	var p Page
	if err := c.Get(ctx, path, q, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ForEachPage walks every page of a collection (newest first) and calls fn with each page's
// records. It returns only after the last page, so a nil error means nothing was skipped.
func (c *Client) ForEachPage(ctx context.Context, path string, start time.Time, fn func([]json.RawMessage) error) error {
	seen := map[string]bool{}
	token := ""
	for {
		p, err := c.ListPage(ctx, path, start, token)
		if err != nil {
			return err
		}
		if err := fn(p.Records); err != nil {
			return err
		}
		if p.NextToken == "" {
			return nil
		}
		if seen[p.NextToken] {
			return fmt.Errorf("whoop: %s pagination returned next_token %q twice", path, p.NextToken)
		}
		seen[p.NextToken] = true
		token = p.NextToken
	}
}

// GetRaw fetches a single JSON document such as /v2/user/profile/basic.
func (c *Client) GetRaw(ctx context.Context, path string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.Get(ctx, path, nil, &raw)
	return raw, err
}

// Get performs an authenticated GET and decodes the JSON response into out.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	token, err := c.Tokens.AccessToken(ctx, "")
	if err != nil {
		return err
	}
	refreshed := false
	backoff := c.RetryBase
	for attempt := 1; ; attempt++ {
		if err := c.wait(ctx); err != nil {
			return err
		}
		status, header, body, err := c.do(ctx, u, token)
		var failure error
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failure = err
		case status >= 200 && status < 300:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("whoop: decoding %s: %w", path, err)
			}
			return nil
		case status == http.StatusUnauthorized && !refreshed:
			refreshed = true
			if token, err = c.Tokens.AccessToken(ctx, token); err != nil {
				return err
			}
			continue
		case status == http.StatusTooManyRequests:
			wait := resetAfter(header, time.Minute)
			if wait > c.MaxWait {
				return fmt.Errorf("%w: WHOOP asks to wait %s before the next request", ErrRateLimited, wait)
			}
			c.delayNext(wait)
			failure = &APIError{Status: status, Path: path, Body: snippet(body)}
		case status >= 500:
			failure = &APIError{Status: status, Path: path, Body: snippet(body)}
		default:
			return &APIError{Status: status, Path: path, Body: snippet(body)}
		}
		if attempt >= c.MaxAttempts {
			return fmt.Errorf("whoop: giving up on %s after %d attempts: %w", path, attempt, failure)
		}
		if status == http.StatusTooManyRequests {
			c.logf("whoop: %s rate limited; waiting before retry", path)
			continue // wait() honours the delay set above
		}
		c.logf("whoop: %s: %v; retrying in %s", path, failure, backoff)
		if err := sleep(ctx, backoff); err != nil {
			return err
		}
		backoff *= 2
	}
}

func (c *Client) do(ctx context.Context, u, token string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	c.requests.Add(1)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	c.observeRateLimit(resp.Header)
	return resp.StatusCode, resp.Header, body, err
}

// wait blocks until the next request is allowed.
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	now := time.Now()
	at := c.notBefore
	if at.Before(now) {
		at = now
	}
	d := at.Sub(now)
	if d > c.MaxWait {
		c.mu.Unlock()
		return fmt.Errorf("%w: next request allowed in %s", ErrRateLimited, d.Round(time.Second))
	}
	c.notBefore = at.Add(c.MinInterval)
	c.mu.Unlock()
	return sleep(ctx, d)
}

func (c *Client) delayNext(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := time.Now().Add(d); t.After(c.notBefore) {
		c.notBefore = t
	}
}

// observeRateLimit pauses proactively when WHOOP reports no requests left in the window.
func (c *Client) observeRateLimit(h http.Header) {
	if rem, ok := leadingInt(h.Get("X-RateLimit-Remaining")); ok && rem <= 0 {
		c.delayNext(resetAfter(h, time.Minute))
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// resetAfter reads X-RateLimit-Reset (seconds until the window resets).
func resetAfter(h http.Header, def time.Duration) time.Duration {
	for _, name := range []string{"X-RateLimit-Reset", "Retry-After"} {
		if n, ok := leadingInt(h.Get(name)); ok {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

func leadingInt(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	return n, err == nil
}

// FormatTime renders a time the way WHOOP's query parameters expect.
func FormatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
