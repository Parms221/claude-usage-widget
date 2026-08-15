// Package history aggregates the local Claude Code transcripts
// (~/.claude/projects/**/*.jsonl) into the per-model and per-day token stats
// the widget shows. Results are cached per file (size+mtime) so only files
// that changed since the last scan are re-parsed.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Stats is the aggregate view over the scanned window.
type Stats struct {
	// ModelTokens maps model id -> total tokens inside [WeekStart, now].
	ModelTokens map[string]int64
	// DailyTokens maps local day (YYYY-MM-DD) -> total tokens.
	DailyTokens map[string]int64
	// StreakDays counts consecutive days with usage ending today (or
	// yesterday when today has none yet). Capped by the scan window.
	StreakDays int
	// StreakCapped is true when the streak hit the scan window edge.
	StreakCapped bool
	ScannedFiles int
	ScannedAt    time.Time
}

// fileAgg is the cached per-file aggregation, bucketed by hour so any weekly
// window can be applied later without re-reading the file.
type fileAgg struct {
	size    int64
	modTime time.Time
	// hour bucket (unix/3600) -> model -> tokens
	buckets map[int64]map[string]int64
	// message keys this file contributed to the global dedup set
	keys map[string]struct{}
}

// Scanner owns the cache. Scan is serialized by an internal mutex.
type Scanner struct {
	mu    sync.Mutex
	cache map[string]*fileAgg
	seen  map[string]struct{} // cross-file message dedup, keys owned per file
}

func NewScanner() *Scanner {
	return &Scanner{cache: map[string]*fileAgg{}, seen: map[string]struct{}{}}
}

type usageLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	RequestID string `json:"requestId"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens         int64 `json:"input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

var assistantMarker = []byte(`"type":"assistant"`)
var usageMarker = []byte(`"usage"`)

// Scan walks the projects dir and returns aggregated stats.
// weekStart bounds the per-model totals; historyDays bounds the file scan.
func (s *Scanner) Scan(weekStart time.Time, historyDays int) (*Stats, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".claude", "projects")
	cutoff := time.Now().AddDate(0, 0, -historyDays)

	s.mu.Lock()
	defer s.mu.Unlock()

	live := map[string]struct{}{}
	count := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		live[path] = struct{}{}
		count++
		prev, ok := s.cache[path]
		if ok && prev.size == info.Size() && prev.modTime.Equal(info.ModTime()) {
			return nil // unchanged, cached aggregate is valid
		}
		s.evict(path)
		s.cache[path] = s.parseFile(path, info)
		return nil
	})

	// Drop cache entries (and their dedup keys) for files out of the window.
	for p := range s.cache {
		if _, ok := live[p]; !ok {
			s.evict(p)
		}
	}

	stats := &Stats{
		ModelTokens:  map[string]int64{},
		DailyTokens:  map[string]int64{},
		ScannedFiles: count,
		ScannedAt:    time.Now(),
	}
	weekStartBucket := weekStart.Unix() / 3600
	for _, agg := range s.cache {
		for bucket, models := range agg.buckets {
			t := time.Unix(bucket*3600, 0).Local()
			day := t.Format("2006-01-02")
			for model, tok := range models {
				stats.DailyTokens[day] += tok
				if bucket >= weekStartBucket {
					stats.ModelTokens[model] += tok
				}
			}
		}
	}

	// Streak: walk back day by day.
	day := time.Now()
	if stats.DailyTokens[day.Format("2006-01-02")] == 0 {
		day = day.AddDate(0, 0, -1) // today has no usage yet; start yesterday
	}
	maxDays := historyDays - 1
	for i := 0; i < maxDays; i++ {
		if stats.DailyTokens[day.Format("2006-01-02")] == 0 {
			break
		}
		stats.StreakDays++
		day = day.AddDate(0, 0, -1)
	}
	stats.StreakCapped = stats.StreakDays >= maxDays
	return stats, nil
}

// evict removes a cached file and releases its dedup keys so a future parse
// (of this or another file) can claim those messages again.
func (s *Scanner) evict(path string) {
	if prev, ok := s.cache[path]; ok {
		for k := range prev.keys {
			delete(s.seen, k)
		}
		delete(s.cache, path)
	}
}

func (s *Scanner) parseFile(path string, info fs.FileInfo) *fileAgg {
	agg := &fileAgg{
		size:    info.Size(),
		modTime: info.ModTime(),
		buckets: map[int64]map[string]int64{},
		keys:    map[string]struct{}{},
	}
	f, err := os.Open(path)
	if err != nil {
		return agg
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 256*1024), 32*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		// Cheap pre-filter before paying for JSON decoding.
		if !bytes.Contains(line, assistantMarker) || !bytes.Contains(line, usageMarker) {
			continue
		}
		var ul usageLine
		if err := json.Unmarshal(line, &ul); err != nil || ul.Type != "assistant" {
			continue
		}
		u := ul.Message.Usage
		total := u.InputTokens + u.OutputTokens + u.CacheCreationTokens + u.CacheReadTokens
		if total == 0 || ul.Message.Model == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, ul.Timestamp)
		if err != nil {
			continue
		}
		// The same API response can be logged in several transcripts
		// (continued sessions); count each message once.
		if ul.Message.ID != "" {
			key := ul.Message.ID + ul.RequestID
			if _, dup := s.seen[key]; dup {
				continue
			}
			s.seen[key] = struct{}{}
			agg.keys[key] = struct{}{}
		}
		bucket := ts.Unix() / 3600
		m := agg.buckets[bucket]
		if m == nil {
			m = map[string]int64{}
			agg.buckets[bucket] = m
		}
		m[ul.Message.Model] += total
	}
	return agg
}
