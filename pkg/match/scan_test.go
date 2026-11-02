package match

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The regexps the scanners replaced (markerSeq and fluffWords are still the
// fallback, in match.go): the scanners must answer exactly what these do.
var (
	parenGroupRef  = regexp.MustCompile(`[\(\[][^\(\)\[\]]*[\)\]]`)
	firstNumberRef = regexp.MustCompile(`\b(\d+(?:\.\d+)?)\b`)
)

func refNumber(re *regexp.Regexp, s string) (float64, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

// checkScanners fails t where a scanner and its regexp disagree on s. The
// marker and fluff scanners are checked on their own path: a string the
// fallback would take is checked with the fallback runes replaced, so the
// scanner itself is what runs.
func checkScanners(t *testing.T, s string) {
	t.Helper()
	if needsFoldFallback(s) {
		s = strings.NewReplacer("ſ", "s", "K", "k").Replace(s)
	}
	if got, want := replaceMarkers(s), markerSeq.ReplaceAllString(s, " "); got != want {
		t.Fatalf("replaceMarkers(%q) = %q, want %q", s, got, want)
	}
	if got, want := replaceFluff(s), fluffWords.ReplaceAllString(s, " "); got != want {
		t.Fatalf("replaceFluff(%q) = %q, want %q", s, got, want)
	}
	got, changed := replaceParenGroupsOnce(s)
	if want := parenGroupRef.ReplaceAllString(s, " "); got != want || changed != (want != s) {
		t.Fatalf("replaceParenGroupsOnce(%q) = %q, %v; want %q", s, got, changed, want)
	}
	gv, gok := findMarker(s)
	if wv, wok := refNumber(markerSeq, s); gv != wv || gok != wok {
		t.Fatalf("findMarker(%q) = %v, %v; want %v, %v", s, gv, gok, wv, wok)
	}
	gv, gok = findFirstNumber(s)
	if wv, wok := refNumber(firstNumberRef, s); gv != wv || gok != wok {
		t.Fatalf("findFirstNumber(%q) = %v, %v; want %v, %v", s, gv, gok, wv, wok)
	}
}

// checkRemove fails t where removeFoldASCII and the general removeFoldRunes
// disagree on an ASCII s and sub.
func checkRemove(t *testing.T, s, sub string) {
	t.Helper()
	if sub == "" || !isASCII(s) || !isASCII(sub) {
		return
	}
	for _, bounded := range []bool{false, true} {
		if got, want := removeFoldASCII(s, sub, bounded), removeFoldRunes(s, sub, bounded); got != want {
			t.Fatalf("removeFoldASCII(%q, %q, %v) = %q, want %q", s, sub, bounded, got, want)
		}
	}
}

// Random strings built from what makes these patterns hard: every keyword in
// either case, digits, fractions, separators, brackets, word and non-word
// neighbours, multi-byte runes and the two runes that take the fallback.
func TestScannersMatchRegexps(t *testing.T) {
	pieces := []string{"book", "Book", "BK", "bk", "vol", "Volume", "VOL", "part", "Pt", "episode", "Ep", "#",
		".", " ", "  ", "\t", "\n", "1", "2", "10", "3.5", "0", ".5", "007", "x", "_", "a", "Z",
		"(", ")", "[", "]", "ſ", "K", "İ", "é", "’", ":", "-", ",",
		"series", "Unabridged", "abridged", "AudioBook", "dramatized", "adaptation", "land", "Land", "the "}
	r := rand.New(rand.NewPCG(1, 2))
	n := 300_000
	if testing.Short() {
		n = 30_000
	}
	for range n {
		var b strings.Builder
		for k := r.IntN(10); k >= 0; k-- {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		s := b.String()
		checkScanners(t, s)
		checkRemove(t, s, pieces[r.IntN(len(pieces))])
		checkRemove(t, s, "land")
	}
}

// The fallback answers as the regexps do for the strings it takes.
func TestScannersFallback(t *testing.T) {
	for _, s := range []string{"BooK 5", "Episode 3 ſeries", "Maſter (BoK 2)"} {
		if !needsFoldFallback(s) {
			t.Fatalf("%q does not take the fallback", s)
		}
		if got, want := replaceMarkers(s), markerSeq.ReplaceAllString(s, " "); got != want {
			t.Errorf("replaceMarkers(%q) = %q, want %q", s, got, want)
		}
		if got, want := replaceFluff(s), fluffWords.ReplaceAllString(s, " "); got != want {
			t.Errorf("replaceFluff(%q) = %q, want %q", s, got, want)
		}
		gv, gok := findMarker(s)
		if wv, wok := refNumber(markerSeq, s); gv != wv || gok != wok {
			t.Errorf("findMarker(%q) = %v, %v; want %v, %v", s, gv, gok, wv, wok)
		}
	}
}

// Every title and series of a metadata release, when AUDIOSILO_MATCH_CORPUS
// names a TSV of its works (id, title, author, series, position), exported from
// the release's meta.sqlite with:
//
//	sqlite3 -readonly -separator $'\t' meta.sqlite "SELECT w.id, w.title, '', COALESCE(s.name, ''),
//	  COALESCE(sw.position, '') FROM works w LEFT JOIN series_works sw ON sw.work_id = w.id
//	  LEFT JOIN series s ON s.id = sw.series_id" > works.tsv
func TestScannersCorpus(t *testing.T) {
	path := os.Getenv("AUDIOSILO_MATCH_CORPUS")
	if path == "" {
		t.Skip("AUDIOSILO_MATCH_CORPUS not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		c := strings.Split(sc.Text(), "\t")
		if len(c) < 5 {
			continue
		}
		title, series, pos := c[1], c[3], c[4]
		for _, s := range []string{title, series, title + " (Book " + pos + ")", series + " " + pos + " - " + title} {
			checkScanners(t, s)
		}
		checkRemove(t, title, series)
		for _, form := range seriesForms(series) {
			checkRemove(t, title, form)
			checkRemove(t, series+" "+pos+": "+title, form)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("checked %d works", n)
}

// BenchmarkBest scores one query against a pool of an author's books, the
// shape the listening import's fuzzy tier runs.
func BenchmarkBest(b *testing.B) {
	series := []string{"The Primal Hunter", "Dungeon Crawler Carl", "He Who Fights with Monsters", "Cradle"}
	for _, n := range []int{10, 50, 200} {
		var books []Book
		for i := range n {
			s := series[i%len(series)]
			books = append(books, Book{Title: fmt.Sprintf("%s %d: The Rise of Something (Book %d) [Unabridged]", s, i, i),
				Author: "Zogarth", Series: s, SeriesIndex: float64(i%20 + 1)})
		}
		q := Query{Title: "The Primal Hunter 7: A LitRPG Adventure", Author: "Zogarth", Series: "Primal Hunter", Sequence: 7, HasSequence: true}
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				Best(books, q)
			}
		})
	}
}
