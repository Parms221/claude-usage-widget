// Package cache persists the last successful usage reading so a widget that
// starts while the endpoint is rate limited still shows real numbers (flagged
// as stale) instead of zeros.
package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/Parms221/claude-usage-widget/internal/api"
)

// MaxAge is how long a cached reading is still worth showing.
const MaxAge = 24 * time.Hour

func path(dir string) string { return filepath.Join(dir, "usage-cache.json") }

// Load returns the cached reading, or nil when there is none, it cannot be
// parsed, or it is older than MaxAge.
func Load(dir string) *api.Usage {
	data, err := os.ReadFile(path(dir))
	if err != nil {
		return nil
	}
	var u api.Usage
	if err := json.Unmarshal(data, &u); err != nil {
		return nil
	}
	if u.FetchedAt.IsZero() || time.Since(u.FetchedAt) > MaxAge {
		return nil
	}
	return &u
}

// Save writes the reading, replacing any previous one.
func Save(dir string, u *api.Usage) error {
	if u == nil {
		return nil
	}
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	tmp := path(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path(dir))
}
