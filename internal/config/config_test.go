package config

import "testing"

func TestDecodeDefaults(t *testing.T) {
	cfg := decode([]byte(`{}`))
	if cfg != defaults() {
		t.Errorf("decode({}) = %+v, quiero defaults %+v", cfg, defaults())
	}
}

func TestDecodeToleratesBOM(t *testing.T) {
	// PowerShell 5.1 escribe UTF-8 con BOM; el loader no debe romperse.
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"theme":"light"}`)...)
	cfg := decode(data)
	if cfg.Theme != "light" {
		t.Errorf("Theme = %q, quiero light (el BOM rompió el parseo)", cfg.Theme)
	}
}

func TestDecodeInvalidJSONFallsBack(t *testing.T) {
	cfg := decode([]byte(`{esto no es json`))
	if cfg != defaults() {
		t.Errorf("JSON inválido debería devolver defaults, dio %+v", cfg)
	}
}

func TestDecodeClampsValues(t *testing.T) {
	cfg := decode([]byte(`{"theme":"banana","pollSeconds":1,"historyDays":2,"marginX":-5}`))
	if cfg.Theme != "auto" {
		t.Errorf("Theme = %q, quiero auto", cfg.Theme)
	}
	if cfg.PollSeconds != MinPollSeconds {
		t.Errorf("PollSeconds = %d, quiero clamp a %d", cfg.PollSeconds, MinPollSeconds)
	}
	if cfg.HistoryDays != 8 {
		t.Errorf("HistoryDays = %d, quiero clamp a 8", cfg.HistoryDays)
	}
	if cfg.MarginX != 0 {
		t.Errorf("MarginX = %d, quiero clamp a 0", cfg.MarginX)
	}
}

func TestDecodeKeepsValidValues(t *testing.T) {
	cfg := decode([]byte(`{"theme":"dark","pollSeconds":120,"overlayTaskbar":false,"planLabel":"Mi plan"}`))
	if cfg.Theme != "dark" || cfg.PollSeconds != 120 || cfg.OverlayTaskbar || cfg.PlanLabel != "Mi plan" {
		t.Errorf("valores válidos alterados: %+v", cfg)
	}
}
