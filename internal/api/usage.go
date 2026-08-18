// Package api talks to Anthropic's OAuth usage endpoint — the same source the
// official /usage screen uses (utilization is already a 0–100 percentage).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const usageURL = "https://api.anthropic.com/api/oauth/usage"

// userAgent identifies the widget instead of Go's default "Go-http-client/1.1".
const userAgent = "claude-usage-widget (+https://github.com/Parms221/claude-usage-widget)"

// ErrAuth signals a 401/403: the token is invalid and needs a CLI refresh.
var ErrAuth = errors.New("token rechazado por el endpoint de uso")

// ErrRateLimited signals a transient 429 from the usage endpoint. Compare with
// errors.Is: the concrete error is *RateLimitError, which carries Retry-After.
var ErrRateLimited = errors.New("endpoint de uso limitado (429)")

// RateLimitError is the 429 returned by the usage endpoint. RetryAfter mirrors
// the header when the server sent a usable one (it often sends "0", which we
// treat as "no hint" — the caller applies its own backoff).
type RateLimitError struct {
	RetryAfter time.Duration
	Body       string
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%v (Retry-After %v)", ErrRateLimited, e.RetryAfter.Round(time.Second))
	}
	return ErrRateLimited.Error()
}

// Is makes errors.Is(err, ErrRateLimited) work for the typed error.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// Bucket is one rate-limit window as reported by the endpoint.
type Bucket struct {
	Utilization float64    `json:"utilization"` // 0–100
	ResetsAt    *time.Time `json:"resetsAt,omitempty"`
}

type rawBucket struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    *string `json:"resets_at"`
}

// Usage aggregates the windows the widget displays. It is also what the
// on-disk cache stores, hence the JSON tags.
type Usage struct {
	FiveHour  *Bucket   `json:"fiveHour,omitempty"`
	SevenDay  *Bucket   `json:"sevenDay,omitempty"`
	FetchedAt time.Time `json:"fetchedAt"`
}

type rawUsage struct {
	FiveHour *rawBucket `json:"five_hour"`
	SevenDay *rawBucket `json:"seven_day"`
}

var client = &http.Client{Timeout: 30 * time.Second}

// FetchUsage queries the endpoint with the Claude Code bearer token.
func FetchUsage(ctx context.Context, accessToken string) (*Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrAuth
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &RateLimitError{
			RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			Body:       strings.TrimSpace(string(body)),
		}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("endpoint de uso devolvió %d: %s", resp.StatusCode, string(body))
	}

	var raw rawUsage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	u := &Usage{FetchedAt: time.Now()}
	u.FiveHour = convert(raw.FiveHour)
	u.SevenDay = convert(raw.SevenDay)
	return u, nil
}

// ParseRetryAfter reads the header in both allowed forms (delta-seconds or an
// HTTP date). Anything missing, malformed or non-positive yields 0.
func ParseRetryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func convert(rb *rawBucket) *Bucket {
	if rb == nil {
		return nil
	}
	b := &Bucket{Utilization: rb.Utilization}
	if rb.ResetsAt != nil {
		if t, err := time.Parse(time.RFC3339Nano, *rb.ResetsAt); err == nil {
			b.ResetsAt = &t
		}
	}
	return b
}
