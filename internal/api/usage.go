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
	"time"
)

const usageURL = "https://api.anthropic.com/api/oauth/usage"

// ErrAuth signals a 401/403: the token is invalid and needs a CLI refresh.
var ErrAuth = errors.New("token rechazado por el endpoint de uso")

// ErrRateLimited signals a transient 429 from the usage endpoint.
var ErrRateLimited = errors.New("endpoint de uso limitado (429)")

// Bucket is one rate-limit window as reported by the endpoint.
type Bucket struct {
	Utilization float64    `json:"utilization"` // 0–100
	ResetsAt    *time.Time `json:"-"`
}

type rawBucket struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    *string `json:"resets_at"`
}

// Usage aggregates the windows the widget displays.
type Usage struct {
	FiveHour  *Bucket
	SevenDay  *Bucket
	FetchedAt time.Time
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

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrAuth
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrRateLimited
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
