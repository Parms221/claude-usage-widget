package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Parms221/claude-usage-widget/internal/api"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	reset := time.Now().Add(3 * time.Hour).Round(time.Second)
	in := &api.Usage{
		FiveHour:  &api.Bucket{Utilization: 42, ResetsAt: &reset},
		SevenDay:  &api.Bucket{Utilization: 68},
		FetchedAt: time.Now().Add(-10 * time.Minute),
	}
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := Load(dir)
	if got == nil {
		t.Fatal("Load devolvió nil tras guardar una lectura reciente")
	}
	if got.FiveHour == nil || got.FiveHour.Utilization != 42 {
		t.Errorf("FiveHour = %+v, quiero 42 %%", got.FiveHour)
	}
	if got.FiveHour.ResetsAt == nil || !got.FiveHour.ResetsAt.Equal(reset) {
		t.Errorf("ResetsAt = %v, quiero %v", got.FiveHour.ResetsAt, reset)
	}
	if got.SevenDay == nil || got.SevenDay.Utilization != 68 {
		t.Errorf("SevenDay = %+v, quiero 68 %%", got.SevenDay)
	}
}

func TestLoadIgnoresStaleAndBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	if got := Load(dir); got != nil {
		t.Error("sin archivo, Load debería dar nil")
	}

	old := &api.Usage{
		FiveHour:  &api.Bucket{Utilization: 10},
		FetchedAt: time.Now().Add(-MaxAge - time.Hour),
	}
	if err := Save(dir, old); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := Load(dir); got != nil {
		t.Error("una lectura más vieja que MaxAge no debe reutilizarse")
	}

	if err := os.WriteFile(filepath.Join(dir, "usage-cache.json"), []byte("{roto"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := Load(dir); got != nil {
		t.Error("un archivo corrupto debe degradar a nil, no romper el arranque")
	}
}

func TestSaveNilIsNoop(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, nil); err != nil {
		t.Fatalf("Save(nil): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "usage-cache.json")); !os.IsNotExist(err) {
		t.Error("Save(nil) no debería crear archivo")
	}
}
