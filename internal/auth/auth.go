//go:build windows

// Package auth reads the Claude Code OAuth credentials and, when the access
// token has expired, asks the official CLI to refresh it (running a minimal
// `claude -p .` just like Claude-Code-Usage-Monitor does). We never call the
// OAuth refresh endpoint ourselves: rotating the refresh token behind Claude
// Code's back could log the user out of the CLI.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Credentials is the claudeAiOauth section of ~/.claude/.credentials.json.
type Credentials struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // unix millis
	Scopes           []string `json:"scopes"`
	SubscriptionType string   `json:"subscriptionType"`
	RateLimitTier    string   `json:"rateLimitTier"`
}

type credFile struct {
	ClaudeAiOauth *Credentials `json:"claudeAiOauth"`
}

// ErrNoCredentials means the user has never logged into Claude Code here.
var ErrNoCredentials = errors.New("no se encontró ~/.claude/.credentials.json (inicia sesión con `claude login`)")

// Path returns the expected credentials file location.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", ".credentials.json"), nil
}

// Load parses the credentials file.
func Load() (*Credentials, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoCredentials
		}
		return nil, err
	}
	var f credFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.ClaudeAiOauth == nil || f.ClaudeAiOauth.AccessToken == "" {
		return nil, ErrNoCredentials
	}
	return f.ClaudeAiOauth, nil
}

// Expired reports whether the access token is (about to be) stale.
func (c *Credentials) Expired() bool {
	if c.ExpiresAt == 0 {
		return false
	}
	return time.Now().UnixMilli() > c.ExpiresAt-60_000
}

// PlanLabel maps subscription metadata to the caption shown in the header.
func (c *Credentials) PlanLabel() string {
	tier := strings.ToLower(c.RateLimitTier)
	switch {
	case strings.Contains(tier, "max_20x"):
		return "Plan Max · 20x"
	case strings.Contains(tier, "max_5x"):
		return "Plan Max · 5x"
	}
	switch strings.ToLower(c.SubscriptionType) {
	case "max":
		return "Plan Max"
	case "pro":
		return "Plan Pro"
	case "team":
		return "Plan Team"
	case "enterprise":
		return "Plan Enterprise"
	}
	return "Claude"
}

// RefreshViaCLI runs `claude -p .` hidden so the CLI renews its own token,
// then the caller should re-Load(). Returns an error if the CLI is missing.
func RefreshViaCLI(ctx context.Context) error {
	path, err := findClaude()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat") {
		cmd = exec.CommandContext(ctx, "cmd", "/c", path, "-p", ".")
	} else {
		cmd = exec.CommandContext(ctx, path, "-p", ".")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	return cmd.Run()
}

func findClaude() (string, error) {
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	// Common install locations when PATH is not inherited (e.g. autostart).
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", "claude.exe"),
		filepath.Join(home, "AppData", "Roaming", "npm", "claude.cmd"),
		filepath.Join(home, "AppData", "Local", "Programs", "claude", "claude.exe"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("no se encontró el CLI de Claude Code en PATH")
}
