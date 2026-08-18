package vm

import (
	"testing"
	"time"

	"github.com/Parms221/claude-usage-widget/internal/api"
)

func TestBuildFlagsMissingData(t *testing.T) {
	v := Build(Inputs{Status: "error", StatusMsg: "Límite de consultas · reintento en 5 min"})
	if v.HasData {
		t.Error("sin lectura HasData debe ser false para que la pastilla muestre —")
	}
	if v.UpdatedAt != "" {
		t.Errorf("UpdatedAt = %q, sin datos no hay hora que mostrar", v.UpdatedAt)
	}
	if v.StatusMsg == "" {
		t.Error("el mensaje de estado debe llegar a la vista")
	}
}

// The timestamp describes the reading, not the render: the view is rebuilt
// every 30 s while a stale reading can be hours old.
func TestBuildUpdatedAtComesFromTheReading(t *testing.T) {
	fetched := time.Now().Add(-3 * time.Hour)
	reset := time.Now().Add(2 * time.Hour)
	v := Build(Inputs{
		Status: "stale",
		Usage: &api.Usage{
			FiveHour:  &api.Bucket{Utilization: 40, ResetsAt: &reset},
			FetchedAt: fetched,
		},
	})
	if !v.HasData {
		t.Fatal("con lectura cacheada HasData debe ser true")
	}
	if want := fetched.Local().Format("15:04"); v.UpdatedAt != want {
		t.Errorf("UpdatedAt = %q, quiero %q (hora de la lectura)", v.UpdatedAt, want)
	}
	if v.SessionPct != 40 {
		t.Errorf("SessionPct = %d, la ventana sigue vigente y debe conservarse", v.SessionPct)
	}
}

// A cached reading whose window already reset must not keep showing the old
// percentage — the window rolled over while we could not poll.
func TestBuildZeroesExpiredWindows(t *testing.T) {
	past := time.Now().Add(-10 * time.Minute)
	v := Build(Inputs{
		Status: "stale",
		Usage: &api.Usage{
			FiveHour:  &api.Bucket{Utilization: 80, ResetsAt: &past},
			SevenDay:  &api.Bucket{Utilization: 65, ResetsAt: &past},
			FetchedAt: time.Now().Add(-6 * time.Hour),
		},
	})
	if v.SessionPct != 0 {
		t.Errorf("SessionPct = %d, la ventana de 5 h ya venció", v.SessionPct)
	}
	if v.WeeklyPct != 0 {
		t.Errorf("WeeklyPct = %d, la ventana semanal ya venció", v.WeeklyPct)
	}
	if v.SessionLive {
		t.Error("una ventana vencida no puede estar activa")
	}
}

func TestExpired(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	if expired(&api.Bucket{}, now) {
		t.Error("sin resets_at no se puede afirmar que venció")
	}
	if !expired(&api.Bucket{ResetsAt: &past}, now) {
		t.Error("resets_at pasado = ventana vencida")
	}
	if expired(&api.Bucket{ResetsAt: &future}, now) {
		t.Error("resets_at futuro = ventana vigente")
	}
}

func TestFmtPct(t *testing.T) {
	if got := FmtPct(42.4); got != "42 %" {
		t.Errorf("FmtPct(42.4) = %q, quiero \"42 %%\"", got)
	}
}
