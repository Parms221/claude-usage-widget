// Package config persists user preferences under %APPDATA%\ClaudeUsageWidget.
package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	// DefaultPollSeconds keeps the widget well inside what the usage endpoint
	// tolerates. Polling every minute earned a permanent 429.
	DefaultPollSeconds = 300
	// MinPollSeconds is the floor for a hand-edited config.json.
	MinPollSeconds = 120
)

// Config holds the widget preferences. Every field has a sensible default so a
// missing or partial config.json never breaks startup.
type Config struct {
	// Theme is "auto", "dark" or "light".
	Theme string `json:"theme"`
	// OverlayTaskbar floats the pill over the taskbar itself (like the design).
	// When false the pill sits just above the work area.
	OverlayTaskbar bool `json:"overlayTaskbar"`
	// MarginX is the left offset of the pill/panel in logical pixels.
	MarginX int `json:"marginX"`
	// PollSeconds is the usage endpoint refresh interval. The endpoint rate
	// limits aggressively (and a 429 keeps renewing itself while you retry),
	// so anything under MinPollSeconds is clamped.
	PollSeconds int `json:"pollSeconds"`
	// HistoryDays controls how far back local JSONL transcripts are scanned
	// (needed for the streak counter; 7-day stats always use the API window).
	HistoryDays int `json:"historyDays"`
	// AutoRefreshCLI runs `claude -p .` to let the official CLI refresh an
	// expired OAuth token (never touches the refresh token ourselves).
	AutoRefreshCLI bool `json:"autoRefreshCli"`
	// Acrylic tries the native Windows 11 blur backdrop behind the panel.
	Acrylic bool `json:"acrylic"`
	// PlanLabel overrides the plan caption shown in the header.
	PlanLabel string `json:"planLabel,omitempty"`
}

func defaults() Config {
	return Config{
		Theme:          "auto",
		OverlayTaskbar: true,
		MarginX:        16,
		PollSeconds:    DefaultPollSeconds,
		HistoryDays:    15,
		AutoRefreshCLI: true,
		Acrylic:        true,
	}
}

// Dir returns the app data directory, creating it if needed.
func Dir() string {
	base, err := os.UserConfigDir() // %APPDATA%
	if err != nil {
		base = "."
	}
	dir := filepath.Join(base, "ClaudeUsageWidget")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func path() string { return filepath.Join(Dir(), "config.json") }

// Load reads config.json, filling gaps with defaults. First run writes the
// default file so users can discover the knobs.
func Load() *Config {
	data, err := os.ReadFile(path())
	if err != nil {
		c := defaults()
		_ = c.Save()
		return &c
	}
	cfg := decode(data)
	return &cfg
}

// decode parses config bytes tolerantly: UTF-8 BOM, unknown or missing
// fields, and out-of-range values all degrade to safe defaults.
func decode(data []byte) Config {
	cfg := defaults()
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}) // editors love BOMs
	if err := json.Unmarshal(data, &cfg); err != nil {
		cfg = defaults()
	}
	if cfg.Theme != "dark" && cfg.Theme != "light" {
		cfg.Theme = "auto"
	}
	if cfg.PollSeconds < MinPollSeconds {
		cfg.PollSeconds = MinPollSeconds
	}
	if cfg.HistoryDays < 8 {
		cfg.HistoryDays = 8
	}
	if cfg.MarginX < 0 {
		cfg.MarginX = 0
	}
	return cfg
}

// Save writes the config atomically enough for our purposes.
func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path(), data, 0o644)
}
