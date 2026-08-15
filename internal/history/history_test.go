package history

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// line builds one assistant transcript line like Claude Code writes them.
func line(ts time.Time, msgID, model string, tokens int64) string {
	return fmt.Sprintf(
		`{"parentUuid":"x","type":"assistant","timestamp":%q,"requestId":"req-%s","message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		ts.UTC().Format(time.RFC3339Nano), msgID, msgID, model, tokens)
}

// dayOf mirrors the scanner's hour-bucket day attribution.
func dayOf(ts time.Time) string {
	return time.Unix(ts.Unix()/3600*3600, 0).Local().Format("2006-01-02")
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	data := ""
	for _, l := range lines {
		data += l + "\n"
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanAggregatesAndDedupes(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	tToday := now.Add(-30 * time.Minute)
	tYesterday := now.Add(-24*time.Hour - 30*time.Minute)
	tOld := now.AddDate(0, 0, -8) // dentro del archivo, fuera de la ventana semanal

	dup := line(tToday, "m1", "claude-opus-4-6", 100)
	write(t, filepath.Join(root, "a.jsonl"),
		dup,
		line(tYesterday, "m2", "claude-sonnet-4-5", 50),
		line(tOld, "m3", "claude-opus-4-6", 999),
		`{"type":"user","message":{"content":"hola"}}`, // sin usage: se ignora
		"esto no es json",
	)
	// b.jsonl repite m1 (sesión continuada) y aporta m4.
	write(t, filepath.Join(root, "b.jsonl"),
		dup,
		line(tToday, "m4", "claude-opus-4-6", 25),
	)

	s := NewScanner()
	s.Root = root
	weekStart := now.AddDate(0, 0, -7)
	st, err := s.Scan(weekStart, 15)
	if err != nil {
		t.Fatal(err)
	}

	if got := st.ModelTokens["claude-opus-4-6"]; got != 125 {
		t.Errorf("opus = %d, quiero 125 (m1 deduplicado, m3 fuera de la semana)", got)
	}
	if got := st.ModelTokens["claude-sonnet-4-5"]; got != 50 {
		t.Errorf("sonnet = %d, quiero 50", got)
	}
	if got := st.DailyTokens[dayOf(tToday)]; got != 125 {
		t.Errorf("hoy = %d, quiero 125", got)
	}
	if got := st.DailyTokens[dayOf(tYesterday)]; got != 50 {
		t.Errorf("ayer = %d, quiero 50", got)
	}
	if st.StreakDays != 2 {
		t.Errorf("racha = %d, quiero 2", st.StreakDays)
	}
	if st.ScannedFiles != 2 {
		t.Errorf("archivos = %d, quiero 2", st.ScannedFiles)
	}
}

func TestScanIncrementalRescan(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	tToday := now.Add(-30 * time.Minute)

	path := filepath.Join(root, "a.jsonl")
	l1 := line(tToday, "m1", "claude-opus-4-6", 100)
	write(t, path, l1)

	s := NewScanner()
	s.Root = root
	weekStart := now.AddDate(0, 0, -7)
	if _, err := s.Scan(weekStart, 15); err != nil {
		t.Fatal(err)
	}

	// El archivo crece (append de Claude Code): el re-parseo no debe perder
	// ni duplicar los mensajes ya vistos.
	write(t, path, l1, line(tToday, "m2", "claude-opus-4-6", 40))
	st, err := s.Scan(weekStart, 15)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.ModelTokens["claude-opus-4-6"]; got != 140 {
		t.Errorf("tras re-scan = %d, quiero 140", got)
	}
}
