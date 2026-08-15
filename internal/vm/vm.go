// Package vm builds the JSON view-model the HTML widget renders. All the
// Spanish-facing formatting ("1,84 M", "en 2 d 14 h", "17:44") lives here so
// the JS side only injects strings and widths.
package vm

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Parms221/claude-usage-widget/internal/api"
	"github.com/Parms221/claude-usage-widget/internal/history"
)

// Model is one row of the "Por modelo" section.
type Model struct {
	Name   string  `json:"name"`
	Tokens string  `json:"tokens"`
	Share  float64 `json:"share"` // % of the weekly limit (bar segment width)
}

// Day is one bar of the "Últimos 7 días" chart.
type Day struct {
	Label  string  `json:"label"`
	Rel    float64 `json:"rel"` // 0-100 relative height
	HasUse bool    `json:"hasUse"`
	Today  bool    `json:"today"`
}

// VM is pushed to the webview after every poll.
type VM struct {
	Status    string `json:"status"` // ok | loading | auth | error
	StatusMsg string `json:"statusMsg,omitempty"`

	Theme     string `json:"theme"`     // resolved: dark | light
	ThemeMode string `json:"themeMode"` // configured: auto | dark | light
	Acrylic   bool   `json:"acrylic"`
	Autostart bool   `json:"autostart"`

	Plan string `json:"plan"`

	WeeklyPct    int    `json:"weeklyPct"`
	TokensUsed   string `json:"tokensUsed"`
	TokensLimit  string `json:"tokensLimit,omitempty"` // "≈ 7,6 M" (estimate)
	ResetLabel   string `json:"resetLabel"`            // "en 2 d 14 h"
	SessionPct   int    `json:"sessionPct"`
	SessionUsed  string `json:"sessionUsed"`  // "2 h 48 min"
	SessionLeft  string `json:"sessionLeft"`  // "3 h 12 min"
	SessionShort string `json:"sessionShort"` // "3 h 12 m" (pill countdown)
	SessionReset string `json:"sessionReset"` // "17:44"
	SessionLive  bool   `json:"sessionLive"`

	Models  []Model `json:"models"`
	FreePct float64 `json:"freePct"`
	Days    []Day   `json:"days"`
	Streak  string  `json:"streak"`

	UpdatedAt string `json:"updatedAt"`
}

// Inputs bundles everything Build needs.
type Inputs struct {
	Usage     *api.Usage
	Hist      *history.Stats
	Plan      string
	ThemeMode string // auto | dark | light
	LightOS   bool   // Windows "apps use light theme"
	Acrylic   bool
	Autostart bool
	Status    string
	StatusMsg string
}

// Build assembles the view-model.
func Build(in Inputs) *VM {
	now := time.Now()
	v := &VM{
		Status:    in.Status,
		StatusMsg: in.StatusMsg,
		ThemeMode: in.ThemeMode,
		Acrylic:   in.Acrylic,
		Autostart: in.Autostart,
		Plan:      in.Plan,
		UpdatedAt: now.Format("15:04"),
	}
	switch in.ThemeMode {
	case "dark":
		v.Theme = "dark"
	case "light":
		v.Theme = "light"
	default:
		if in.LightOS {
			v.Theme = "light"
		} else {
			v.Theme = "dark"
		}
	}

	if in.Usage != nil {
		buildUsage(v, in.Usage, now)
	}
	if in.Hist != nil {
		buildHistory(v, in.Usage, in.Hist, now)
	}
	return v
}

func buildUsage(v *VM, u *api.Usage, now time.Time) {
	if w := u.SevenDay; w != nil {
		v.WeeklyPct = int(math.Round(w.Utilization))
		if w.ResetsAt != nil {
			v.ResetLabel = "en " + FmtDur(time.Until(*w.ResetsAt))
		}
	}
	if s := u.FiveHour; s != nil {
		v.SessionPct = int(math.Round(s.Utilization))
		if s.ResetsAt != nil && s.ResetsAt.After(now) {
			left := time.Until(*s.ResetsAt)
			if left > 5*time.Hour {
				left = 5 * time.Hour
			}
			used := 5*time.Hour - left
			v.SessionLeft = FmtDur(left)
			v.SessionShort = FmtDurPill(left)
			v.SessionUsed = FmtDur(used)
			v.SessionReset = s.ResetsAt.Local().Format("15:04")
			v.SessionLive = s.Utilization > 0
		}
	}
}

func buildHistory(v *VM, u *api.Usage, h *history.Stats, now time.Time) {
	// ---- per-model over the weekly window ----
	var total int64
	type mt struct {
		id  string
		tok int64
	}
	var list []mt
	for id, tok := range h.ModelTokens {
		total += tok
		list = append(list, mt{id, tok})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].tok > list[j].tok })

	if total > 0 {
		v.TokensUsed = FmtTokens(total)
		weekly := float64(v.WeeklyPct)
		if weekly >= 2 {
			est := float64(total) * 100 / weekly
			v.TokensLimit = "≈ " + FmtTokens(int64(est))
		}
		shown := list
		var rest int64
		if len(list) > 3 {
			shown = list[:3]
			for _, m := range list[3:] {
				rest += m.tok
			}
		}
		for _, m := range shown {
			v.Models = append(v.Models, Model{
				Name:   ModelDisplayName(m.id),
				Tokens: FmtTokens(m.tok),
				Share:  weekly * float64(m.tok) / float64(total),
			})
		}
		if rest > 0 {
			v.Models = append(v.Models, Model{
				Name:   "Otros",
				Tokens: FmtTokens(rest),
				Share:  weekly * float64(rest) / float64(total),
			})
		}
	}
	v.FreePct = math.Max(0, 100-float64(v.WeeklyPct))

	// ---- last 7 calendar days ----
	labels := []string{"L", "M", "M", "J", "V", "S", "D"} // Mon..Sun like the design
	var max int64
	vals := make([]int64, 7)
	for i := 0; i < 7; i++ {
		day := now.AddDate(0, 0, i-6)
		vals[i] = h.DailyTokens[day.Format("2006-01-02")]
		if vals[i] > max {
			max = vals[i]
		}
	}
	for i := 0; i < 7; i++ {
		day := now.AddDate(0, 0, i-6)
		wd := (int(day.Weekday()) + 6) % 7 // Monday=0
		rel := 0.0
		if max > 0 {
			rel = float64(vals[i]) * 100 / float64(max)
		}
		v.Days = append(v.Days, Day{
			Label:  labels[wd],
			Rel:    rel,
			HasUse: vals[i] > 0,
			Today:  i == 6,
		})
	}

	// ---- streak ----
	switch {
	case h.StreakDays <= 0:
		v.Streak = "Sin racha"
	case h.StreakCapped:
		v.Streak = fmt.Sprintf("Racha de %d+ días", h.StreakDays)
	case h.StreakDays == 1:
		v.Streak = "Racha de 1 día"
	default:
		v.Streak = fmt.Sprintf("Racha de %d días", h.StreakDays)
	}
}

// ModelDisplayName turns "claude-opus-4-6-20260115" into "Opus 4.6".
func ModelDisplayName(id string) string {
	s := strings.ToLower(id)
	s = strings.TrimPrefix(s, "claude-")
	families := []string{"fable", "mythos", "opus", "sonnet", "haiku"}
	for _, fam := range families {
		if idx := strings.Index(s, fam); idx >= 0 {
			rest := strings.TrimPrefix(s[idx+len(fam):], "-")
			parts := strings.Split(rest, "-")
			version := ""
			if len(parts) > 0 && parts[0] != "" && isDigits(parts[0]) {
				version = parts[0]
				if len(parts) > 1 && isDigits(parts[1]) && len(parts[1]) < 3 {
					version += "." + parts[1]
				}
			}
			name := strings.ToUpper(fam[:1]) + fam[1:]
			if version != "" {
				return name + " " + version
			}
			return name
		}
	}
	if id == "" {
		return "Desconocido"
	}
	return id
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// FmtTokens renders token counts the way the design does: "620 K", "1,84 M".
func FmtTokens(n int64) string {
	f := float64(n)
	switch {
	case f >= 1e9:
		return comma(sig(f/1e9)) + " B"
	case f >= 1e6:
		return comma(sig(f/1e6)) + " M"
	case f >= 1e3:
		return fmt.Sprintf("%.0f K", f/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// sig keeps ~3 significant digits: 1.84, 12.3, 123.
func sig(f float64) string {
	switch {
	case f < 10:
		return fmt.Sprintf("%.2f", f)
	case f < 100:
		return fmt.Sprintf("%.1f", f)
	default:
		return fmt.Sprintf("%.0f", f)
	}
}

func comma(s string) string { return strings.ReplaceAll(s, ".", ",") }

// FmtDur renders "2 d 14 h", "3 h 12 min" or "45 min".
func FmtDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	mins := int(d.Round(time.Minute) / time.Minute)
	days := mins / (24 * 60)
	hours := (mins % (24 * 60)) / 60
	m := mins % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%d d %d h", days, hours)
	case hours > 0:
		return fmt.Sprintf("%d h %d min", hours, m)
	default:
		return fmt.Sprintf("%d min", m)
	}
}

// FmtDurShort renders "2 d", "14 h" or "45 min".
func FmtDurShort(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	mins := int(d.Round(time.Minute) / time.Minute)
	switch {
	case mins >= 24*60:
		return fmt.Sprintf("%d d", mins/(24*60))
	case mins >= 60:
		return fmt.Sprintf("%d h", mins/60)
	default:
		return fmt.Sprintf("%d min", mins)
	}
}

// FmtDurPill is the compact countdown shown in the taskbar pill:
// "3 h 12 m", "3 h", "45 min".
func FmtDurPill(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	mins := int(d.Round(time.Minute) / time.Minute)
	if mins >= 60 {
		if m := mins % 60; m != 0 {
			return fmt.Sprintf("%d h %d m", mins/60, m)
		}
		return fmt.Sprintf("%d h", mins/60)
	}
	return fmt.Sprintf("%d min", mins)
}
