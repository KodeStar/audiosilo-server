package library

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkOpenPodcast times opening a downloaded episode for one stream
// request (each Range request opens it again) in a show whose feed.json holds
// AUDIOSILO_PERF_EPISODES episodes (1,000) with real-sized show notes
// (server#180).
//
//	go test ./internal/library -run '^$' -bench BenchmarkOpenPodcast
func BenchmarkOpenPodcast(b *testing.B) {
	ctx, cat, lib, s, f, dir := podcastDiskFixture(b)
	ep := f.Episodes[0]
	writePodcastFixture(b, filepath.Join(lib.Root, filepath.FromSlash(ep.Path)), "chap-v24-reverse.mp3")
	raw, err := os.ReadFile(filepath.Join(dir, "feed.json"))
	if err != nil {
		b.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		b.Fatal(err)
	}
	eps := doc["episodes"].([]any)
	tmpl, _ := json.Marshal(eps[len(eps)-1])
	notes := strings.Repeat("Show notes with links and credits. ", 60) // ~2 KB, as a real feed's
	for i := len(eps); i < 1000; i++ {
		var e map[string]any
		_ = json.Unmarshal(tmpl, &e)
		id := fmt.Sprintf("%016x", 0xabc0000+i)
		e["id"], e["guid"], e["description"] = id, "bench-"+id, notes
		e["path"] = fmt.Sprintf("Fixture Show/2020-01-01-%s.mp3", id[:10])
		e["fetch"] = nil
		eps = append(eps, e)
	}
	doc["episodes"] = eps
	big, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "feed.json"), big, 0o644); err != nil {
		b.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(len(big))/1024, "feed_KB")
	b.ReportAllocs()
	for b.Loop() {
		opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
		if err != nil {
			b.Fatal(err)
		}
		_ = opened.Close()
	}
}
