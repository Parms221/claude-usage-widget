package vm

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Parms221/claude-usage-widget/internal/api"
	"github.com/Parms221/claude-usage-widget/internal/history"
)

func TestFmtTokens(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1 K"},
		{620_000, "620 K"},
		{1_840_000, "1,84 M"},
		{12_300_000, "12,3 M"},
		{123_000_000, "123 M"},
		{1_840_000_000, "1,84 B"},
	}
	for _, c := range cases {
		if got := FmtTokens(c.in); got != c.want {
			t.Errorf("FmtTokens(%d) = %q, quiero %q", c.in, got, c.want)
		}
	}
}

func TestFmtDur(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-5 * time.Minute, "0 min"},
		{45 * time.Minute, "45 min"},
		{3*time.Hour + 12*time.Minute, "3 h 12 min"},
		{62 * time.Hour, "2 d 14 h"},
	}
	for _, c := range cases {
		if got := FmtDur(c.in); got != c.want {
			t.Errorf("FmtDur(%v) = %q, quiero %q", c.in, got, c.want)
		}
	}
}

func TestFmtDurShort(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{45 * time.Minute, "45 min"},
		{90 * time.Minute, "1 h"},
		{3 * 24 * time.Hour, "3 d"},
	}
	for _, c := range cases {
		if got := FmtDurShort(c.in); got != c.want {
			t.Errorf("FmtDurShort(%v) = %q, quiero %q", c.in, got, c.want)
		}
	}
}

func TestFmtDurPill(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{45 * time.Minute, "45 min"},
		{3 * time.Hour, "3 h"},
		{3*time.Hour + 12*time.Minute, "3 h 12 m"},
	}
	for _, c := range cases {
		if got := FmtDurPill(c.in); got != c.want {
			t.Errorf("FmtDurPill(%v) = %q, quiero %q", c.in, got, c.want)
		}
	}
}

func TestModelDisplayName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"claude-fable-5", "Fable 5"},
		{"claude-opus-4-6-20260115", "Opus 4.6"},
		{"claude-sonnet-4-5-20250929", "Sonnet 4.5"},
		{"claude-haiku-4-5-20251001", "Haiku 4.5"},
		{"gpt-4", "gpt-4"},
		{"", "Desconocido"},
	}
	for _, c := range cases {
		if got := ModelDisplayName(c.in); got != c.want {
			t.Errorf("ModelDisplayName(%q) = %q, quiero %q", c.in, got, c.want)
		}
	}
}

func TestBuildThemeResolution(t *testing.T) {
	cases := []struct {
		mode    string
		lightOS bool
		want    string
	}{
		{"dark", true, "dark"},
		{"light", false, "light"},
		{"auto", true, "light"},
		{"auto", false, "dark"},
	}
	for _, c := range cases {
		v := Build(Inputs{ThemeMode: c.mode, LightOS: c.lightOS, Status: "ok"})
		if v.Theme != c.want {
			t.Errorf("Build(mode=%s lightOS=%v).Theme = %q, quiero %q", c.mode, c.lightOS, v.Theme, c.want)
		}
	}
}

func TestBuildUsageAndHistory(t *testing.T) {
	now := time.Now()
	r5 := now.Add(2*time.Hour + 12*time.Minute + 5*time.Second)
	r7 := now.Add(62*time.Hour + 30*time.Second)
	usage := &api.Usage{
		FiveHour: &api.Bucket{Utilization: 56, ResetsAt: &r5},
		SevenDay: &api.Bucket{Utilization: 68, ResetsAt: &r7},
	}
	hist := &history.Stats{
		ModelTokens: map[string]int64{
			"claude-opus-4-6":   100_000,
			"claude-sonnet-4-5": 50_000,
			"claude-haiku-4-5":  30_000,
			"claude-fable-5":    10_000,
			"claude-mythos-5":   10_000,
		},
		DailyTokens: map[string]int64{now.Format("2006-01-02"): 200_000},
		StreakDays:  6,
	}
	v := Build(Inputs{Usage: usage, Hist: hist, ThemeMode: "dark", Status: "ok"})

	if v.WeeklyPct != 68 || v.SessionPct != 56 {
		t.Fatalf("pct = %d/%d, quiero 68/56", v.WeeklyPct, v.SessionPct)
	}
	if v.SessionUsed != "2 h 48 min" || v.SessionLeft != "2 h 12 min" || v.SessionShort != "2 h 12 m" {
		t.Errorf("sesión: used=%q left=%q short=%q", v.SessionUsed, v.SessionLeft, v.SessionShort)
	}
	if !v.SessionLive {
		t.Error("SessionLive debería ser true con utilización > 0")
	}
	if v.ResetLabel != "en 2 d 14 h" {
		t.Errorf("ResetLabel = %q", v.ResetLabel)
	}
	if v.FreePct != 32 {
		t.Errorf("FreePct = %v, quiero 32", v.FreePct)
	}
	if !strings.HasPrefix(v.TokensLimit, "≈ ") {
		t.Errorf("TokensLimit = %q, quiero estimado con prefijo", v.TokensLimit)
	}

	// 5 modelos → top 3 + "Otros"; las porciones deben sumar el % semanal.
	if len(v.Models) != 4 {
		t.Fatalf("len(Models) = %d, quiero 4 (top 3 + Otros)", len(v.Models))
	}
	if v.Models[0].Name != "Opus 4.6" || v.Models[3].Name != "Otros" {
		t.Errorf("orden de modelos: %v", v.Models)
	}
	var sum float64
	for _, m := range v.Models {
		sum += m.Share
	}
	if math.Abs(sum-68) > 0.01 {
		t.Errorf("suma de shares = %v, quiero 68", sum)
	}

	if len(v.Days) != 7 || !v.Days[6].Today {
		t.Fatalf("Days mal formado: len=%d", len(v.Days))
	}
	if !v.Days[6].HasUse || v.Days[6].Rel != 100 {
		t.Errorf("hoy debería tener uso al 100%%: %+v", v.Days[6])
	}
	if v.Streak != "Racha de 6 días" {
		t.Errorf("Streak = %q", v.Streak)
	}
}

func TestBuildStreakLabels(t *testing.T) {
	cases := []struct {
		days   int
		capped bool
		want   string
	}{
		{0, false, "Sin racha"},
		{1, false, "Racha de 1 día"},
		{5, false, "Racha de 5 días"},
		{14, true, "Racha de 14+ días"},
	}
	for _, c := range cases {
		h := &history.Stats{
			ModelTokens: map[string]int64{},
			DailyTokens: map[string]int64{},
			StreakDays:  c.days, StreakCapped: c.capped,
		}
		v := Build(Inputs{Hist: h, ThemeMode: "dark", Status: "ok"})
		if v.Streak != c.want {
			t.Errorf("streak(%d,%v) = %q, quiero %q", c.days, c.capped, v.Streak, c.want)
		}
	}
}
