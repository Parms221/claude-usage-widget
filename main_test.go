//go:build windows

package main

import (
	"testing"
	"time"

	"github.com/Parms221/claude-usage-widget/internal/api"
	"github.com/Parms221/claude-usage-widget/internal/config"
)

func TestBackoffForGrowsAndCaps(t *testing.T) {
	cases := []struct {
		strikes int
		want    time.Duration
	}{
		{0, firstBackoff}, // defensivo: nunca menos que el primer backoff
		{1, 5 * time.Minute},
		{2, 10 * time.Minute},
		{3, 20 * time.Minute},
		{4, 40 * time.Minute},
		{5, maxBackoff},
		{9, maxBackoff},
		{100, maxBackoff},
	}
	for _, c := range cases {
		if got := backoffFor(c.strikes); got != c.want {
			t.Errorf("backoffFor(%d) = %v, quiero %v", c.strikes, got, c.want)
		}
	}
}

func TestJitterStaysInRange(t *testing.T) {
	base := 10 * time.Minute
	lo, hi := base-base/6, base+base/6
	for i := 0; i < 200; i++ {
		got := jitter(base)
		if got < lo || got > hi {
			t.Fatalf("jitter(%v) = %v, fuera de [%v, %v]", base, got, lo, hi)
		}
	}
	if jitter(0) != 0 {
		t.Error("jitter(0) debería ser 0")
	}
}

// A 429 must space the retries out; a manual refresh may retry without pushing
// the automatic schedule into an hour-long silence.
func TestNoteRateLimitedBackoff(t *testing.T) {
	a := &app{}

	first := a.noteRateLimited(0, false)
	if first < 4*time.Minute || first > 6*time.Minute {
		t.Errorf("primer backoff = %v, quiero ~5 min", first)
	}
	if !a.rateLimited || a.rlStrikes != 1 {
		t.Errorf("estado tras el primer 429: rateLimited=%v strikes=%d", a.rateLimited, a.rlStrikes)
	}

	second := a.noteRateLimited(0, false)
	if second < 8*time.Minute {
		t.Errorf("segundo backoff = %v, debería doblar al primero", second)
	}

	// Un reintento manual no sube el contador ni acorta la espera vigente.
	before := a.backoffUntil
	strikes := a.rlStrikes
	if got := a.noteRateLimited(0, true); got <= 0 {
		t.Error("un reintento manual debe seguir dejando una espera pendiente")
	}
	if a.rlStrikes != strikes {
		t.Errorf("strikes = %d tras un reintento manual, quiero %d", a.rlStrikes, strikes)
	}
	if a.backoffUntil.Before(before) {
		t.Error("un reintento manual no debe acortar el backoff en curso")
	}
}

// Retry-After wins when the server asks for longer than our own backoff.
func TestNoteRateLimitedHonoursRetryAfter(t *testing.T) {
	a := &app{}
	got := a.noteRateLimited(90*time.Minute, false)
	if got < 89*time.Minute {
		t.Errorf("backoff = %v, quiero respetar el Retry-After de 90 min", got)
	}
}

func TestClearRateLimit(t *testing.T) {
	a := &app{}
	a.noteRateLimited(0, false)
	a.clearRateLimit()
	if a.rateLimited || a.rlStrikes != 0 || !a.backoffUntil.IsZero() {
		t.Errorf("clearRateLimit dejó estado: rateLimited=%v strikes=%d hasta=%v",
			a.rateLimited, a.rlStrikes, a.backoffUntil)
	}
}

// While rate limited the widget must keep showing the cached numbers instead
// of blanking the panel — that was the bug users saw as a stuck "Límite de
// consultas · reintentando".
func TestDegradedStatusKeepsCachedData(t *testing.T) {
	a := &app{}
	if got := a.degradedStatus(); got != "error" {
		t.Errorf("sin datos previos quiero \"error\", dio %q", got)
	}
	a.usage = &api.Usage{FetchedAt: time.Now()}
	if got := a.degradedStatus(); got != "stale" {
		t.Errorf("con datos previos quiero \"stale\", dio %q", got)
	}
}

// The scheduler must never retry sooner than the pending backoff.
func TestUntilNextPollRespectsBackoff(t *testing.T) {
	a := &app{cfg: &config.Config{PollSeconds: 300}}
	a.lastAttempt = time.Now()

	if d := a.untilNextPoll(); d > 5*time.Minute+time.Second || d < 4*time.Minute {
		t.Errorf("intervalo normal = %v, quiero ~5 min", d)
	}

	a.backoffUntil = time.Now().Add(20 * time.Minute)
	if d := a.untilNextPoll(); d < 19*time.Minute {
		t.Errorf("con backoff pendiente = %v, quiero ~20 min", d)
	}

	// Ventana de 5 h vacía: nada que refrescar, se espacia el sondeo.
	a.backoffUntil = time.Time{}
	a.usage = &api.Usage{FiveHour: &api.Bucket{Utilization: 0}}
	if d := a.untilNextPoll(); d < 14*time.Minute {
		t.Errorf("sin actividad = %v, quiero ~15 min (300 s x %d)", d, idlePollFactor)
	}
}

func TestManualWaitCooldown(t *testing.T) {
	a := &app{}
	if got := a.manualWait(); got > 0 {
		t.Errorf("sin intentos previos no debería haber enfriamiento, dio %v", got)
	}
	a.lastAttempt = time.Now()
	if got := a.manualWait(); got <= 0 || got > manualCooldown {
		t.Errorf("enfriamiento = %v, quiero <= %v y > 0", got, manualCooldown)
	}
}
