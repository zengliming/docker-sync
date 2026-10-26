package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// scheme returns the URL scheme used for a registry endpoint.
func (c *Client) scheme() string {
	if c.opts.Insecure {
		return "http"
	}
	return "https"
}

// url builds an absolute registry API URL.
func (c *Client) url(ref Ref, path string) string {
	return c.scheme() + "://" + ref.Endpoint() + path
}

// do performs a GET request against the registry, adding Accept headers and
// authentication. It transparently handles bearer token challenges and retries
// throttled or transient failures. A successful response is returned with an
// open body; the caller must close it. Non-2xx responses are returned as-is so
// callers can include the registry error body in their own error messages.
func (c *Client) do(ctx context.Context, ref Ref, rawURL string, accept []string) (*http.Response, error) {
	return c.doMethod(ctx, http.MethodGet, ref, rawURL, accept)
}

// doMethod is do for an arbitrary HTTP method. HEAD requests are used to probe
// blob existence without transferring the payload.
func (c *Client) doMethod(ctx context.Context, method string, ref Ref, rawURL string, accept []string) (*http.Response, error) {
	const maxAttempts = 4
	authTried := false
	attempt := 0
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("registry: build request: %w", err)
		}
		req.Header.Set("User-Agent", c.opts.UserAgent)
		for _, a := range accept {
			req.Header.Add("Accept", a)
		}
		if len(accept) == 0 {
			req.Header.Set("Accept", "*/*")
		}
		if h := c.cachedAuth(ref); h != "" {
			req.Header.Set("Authorization", h)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if attempt >= maxAttempts-1 {
				return nil, fmt.Errorf("registry: %s %s: %w", method, rawURL, lastErr)
			}
			attempt++
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized && !authTried {
			challenge := resp.Header.Get("Www-Authenticate")
			drainAndClose(resp)
			authTried = true
			if err := c.authenticate(ctx, ref, challenge); err != nil {
				return nil, err
			}
			continue
		}
		if retryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			wait := backoff(attempt + 1)
			if d := retryAfter(resp.Header.Get("Retry-After")); d > wait {
				wait = d
			}
			drainAndClose(resp)
			attempt++
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}
		return resp, nil
	}
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d + time.Duration(rand.Int63n(int64(250*time.Millisecond)))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
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

func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		d := time.Duration(secs) * time.Second
		if d > time.Minute {
			d = time.Minute
		}
		return d
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			if d > time.Minute {
				d = time.Minute
			}
			return d
		}
	}
	return 0
}

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// cachedAuth returns a cached Authorization header value for the repository.
func (c *Client) cachedAuth(ref Ref) string {
	host := ref.Endpoint()
	key := host + "|" + ref.Repository
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tokens[key]; ok {
		if time.Now().Before(t.expires) {
			return "Bearer " + t.value
		}
		delete(c.tokens, key)
	}
	if h, ok := c.basic[host]; ok {
		return h
	}
	return ""
}

// authenticate reacts to a WWW-Authenticate challenge.
func (c *Client) authenticate(ctx context.Context, ref Ref, challenge string) error {
	scheme, params := parseChallenge(challenge)
	host := ref.Endpoint()
	switch scheme {
	case "basic":
		if c.opts.Username == "" {
			return fmt.Errorf("registry %s requires credentials", host)
		}
		h := "Basic " + base64.StdEncoding.EncodeToString([]byte(c.opts.Username+":"+c.opts.Password))
		c.mu.Lock()
		c.basic[host] = h
		c.mu.Unlock()
		return nil
	case "bearer":
		realm := params["realm"]
		if realm == "" {
			return fmt.Errorf("registry %s: bearer challenge without realm", host)
		}
		scope := params["scope"]
		if scope == "" {
			scope = "repository:" + ref.Repository + ":pull"
		}
		tok, expires, err := c.fetchToken(ctx, realm, params["service"], scope)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.tokens[host+"|"+ref.Repository] = token{value: tok, expires: expires}
		c.mu.Unlock()
		return nil
	case "":
		return fmt.Errorf("registry %s: 401 without WWW-Authenticate header", host)
	default:
		return fmt.Errorf("registry %s: unsupported auth scheme %q", host, scheme)
	}
}

// fetchToken performs the token exchange described by a bearer challenge.
func (c *Client) fetchToken(ctx context.Context, realm, service, scope string) (string, time.Time, error) {
	u, err := url.Parse(realm)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("registry: bad token realm %q: %w", realm, err)
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	q.Set("scope", scope)
	if c.opts.Username != "" {
		q.Set("account", c.opts.Username)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("User-Agent", c.opts.UserAgent)
	req.Header.Set("Accept", "application/json")
	if c.opts.Username != "" {
		req.SetBasicAuth(c.opts.Username, c.opts.Password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("registry: token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("registry: token endpoint %s: %s: %s", realm, resp.Status, snippet(body))
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("registry: decode token response: %w", err)
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if tok == "" {
		return "", time.Time{}, fmt.Errorf("registry: token endpoint %s returned an empty token", realm)
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if ttl > 30*time.Second {
		ttl -= 30 * time.Second
	}
	return tok, time.Now().Add(ttl), nil
}

// parseChallenge parses a WWW-Authenticate header into its scheme and
// parameters, honouring quoted values that may contain commas.
func parseChallenge(h string) (string, map[string]string) {
	h = strings.TrimSpace(h)
	params := map[string]string{}
	sp := strings.IndexByte(h, ' ')
	if sp < 0 {
		return strings.ToLower(h), params
	}
	scheme := strings.ToLower(strings.TrimSpace(h[:sp]))
	rest := h[sp+1:]
	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, " \t,")
		if rest == "" {
			break
		}
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(rest[:eq]))
		rest = rest[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(rest); i++ {
				if rest[i] == '\\' && i+1 < len(rest) {
					b.WriteByte(rest[i+1])
					i++
					continue
				}
				if rest[i] == '"' {
					break
				}
				b.WriteByte(rest[i])
			}
			val = b.String()
			if i < len(rest) {
				rest = rest[i+1:]
			} else {
				rest = ""
			}
		} else {
			if comma := strings.IndexByte(rest, ','); comma >= 0 {
				val = strings.TrimSpace(rest[:comma])
				rest = rest[comma:]
			} else {
				val = strings.TrimSpace(rest)
				rest = ""
			}
		}
		if key != "" {
			params[key] = val
		}
	}
	return scheme, params
}

// snippet trims a response body for inclusion in an error message.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	return s
}
