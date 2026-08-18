//go:build windows

package main

import (
	"testing"
	"time"

	"github.com/Parms221/claude-usage-widget/internal/config"
)

// Restarting the widget with a fresh cached reading must not fire a request:
// a burst of restarts was one of the ways the 429 got started.
func TestPollIsDue(t *testing.T) {
	a := &app{cfg: &config.Config{PollSeconds: config.DefaultPollSeconds}}
	if !a.pollIsDue() {
		t.Error("sin lectura previa hay que consultar de inmediato")
	}

	a.lastAttempt = time.Now().Add(-30 * time.Second) // caché recién escrita
	if a.pollIsDue() {
		t.Error("con una lectura de hace 30 s no hace falta consultar al arrancar")
	}

	a.lastAttempt = time.Now().Add(-10 * time.Minute)
	if !a.pollIsDue() {
		t.Error("con una lectura de hace 10 min sí toca consultar")
	}
}

// A gather that dies before the HTTP request (no credentials) must still count
// as an attempt, or the loop would spin once per second forever.
func TestGatherStampsAttemptWithoutCredentials(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir()) // sin ~/.claude/.credentials.json
	t.Setenv("HOME", t.TempDir())

	a := &app{cfg: &config.Config{PollSeconds: config.DefaultPollSeconds}}
	a.gather(false)

	if a.lastAttempt.IsZero() {
		t.Fatal("gather sin credenciales dejó lastAttempt sin marcar")
	}
	if a.status != "auth" {
		t.Errorf("status = %q, quiero auth", a.status)
	}
	if d := a.untilNextPoll(); d < time.Minute {
		t.Errorf("próximo intento en %v: el bucle giraría en vacío", d)
	}
}
