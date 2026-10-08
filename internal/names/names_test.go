package names

import (
	"reflect"
	"sort"
	"testing"
)

// TestSplit pins the contributor-splitting rule: unambiguous joiners always
// split, a comma splits only when every part still looks like a full name, and a
// single name is never broken up.
func TestSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"Brandon Sanderson", []string{"Brandon Sanderson"}},
		{"  Brandon Sanderson  ", []string{"Brandon Sanderson"}},
		// The comma here is part of ONE name: "pere" is a single word.
		{"Alexandre Dumas, pere", []string{"Alexandre Dumas, pere"}},
		{"Dumas, Alexandre", []string{"Dumas, Alexandre"}},
		// A particle surname written first is one name, not "Le Guin" and "Ursula K.".
		{"Le Guin, Ursula K.", []string{"Le Guin, Ursula K."}},
		{"Le Guin, Ursula K.; Ann Leckie", []string{"Le Guin, Ursula K.", "Ann Leckie"}},
		{"Martin Luther King, Jr.", []string{"Martin Luther King, Jr."}},
		// Unambiguous joiners.
		{"Terry Pratchett & Neil Gaiman", []string{"Terry Pratchett", "Neil Gaiman"}},
		{"Terry Pratchett and Neil Gaiman", []string{"Terry Pratchett", "Neil Gaiman"}},
		{"A; B", []string{"A", "B"}},
		{"A;B;C", []string{"A", "B", "C"}},
		// Every comma part has two words, so the comma splits.
		{"Terry Pratchett, Neil Gaiman", []string{"Terry Pratchett", "Neil Gaiman"}},
		{"Terry Pratchett, Neil Gaiman, and Rob Wilkins",
			[]string{"Terry Pratchett", "Neil Gaiman", "Rob Wilkins"}},
		// Mixed joiners, and the comma rule applied per chunk.
		{"Jane Doe; John Roe, Ann Poe", []string{"Jane Doe", "John Roe", "Ann Poe"}},
		{"Jane Doe; Alexandre Dumas, pere", []string{"Jane Doe", "Alexandre Dumas, pere"}},
		// Trailing/duplicated/stacked separators collapse rather than yielding
		// blanks or a name beginning with a stranded joiner.
		{"A & B;", []string{"A", "B"}},
		{"Jane Doe; and John Roe", []string{"Jane Doe", "John Roe"}},
		// A half-empty "Last, First" tag: the dangling comma is trimmed off both
		// ends rather than exported as part of the name.
		{", Jane Doe", []string{"Jane Doe"}},
		{"Jane Doe,", []string{"Jane Doe"}},
	}
	for _, tc := range cases {
		if got := Split(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Split(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

// TestSortKey pins the surname-first reading of a credit.
func TestSortKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"Brandon Sanderson", "sanderson brandon"},
		{"J.R.R. Tolkien", "tolkien jrr"},
		{"J. R. R. Tolkien", "tolkien jrr"},
		{"Ursula K. Le Guin", "leguin ursulak"},
		{"Ludwig van Beethoven", "vanbeethoven ludwig"},
		// Two words: the first is a given name, even when it looks like a particle.
		{"Van Morrison", "morrison van"},
		{"Carroll, Lewis", "carroll lewis"},
		{"Sanderson, Brandon", "sanderson brandon"},
		// A particle surname written first files where the natural form does.
		{"Le Guin, Ursula K.", "leguin ursulak"},
		{"van Beethoven, Ludwig", "vanbeethoven ludwig"},
		{"Alexandre Dumas, pere", "dumas alexandre"},
		{"Martin Luther King, Jr.", "king martinluther"},
		{"Martin Luther King Jr.", "king martinluther"},
		// A suffix after "Surname, Given" keeps the given names in the key.
		{"King, Martin Luther, Jr.", "king martinluther"},
		{"Doe, John, Jr.", "doe john"},
		{"Plato", "plato "},
		// A co-credit sorts by its first name.
		{"Michael Kramer, Kate Reading", "kramer michael"},
		{"Terry Pratchett & Neil Gaiman", "pratchett terry"},
		{"Émile Zola", "zola émile"},
		{"夏目漱石", "夏目漱石 "},
	}
	for _, tc := range cases {
		if got := SortKey(tc.in); got != tc.want {
			t.Errorf("SortKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSortKeyOrder checks the keys order a shelf the way a reader expects: every
// Smith before Smithers, and Le Guin under L.
func TestSortKeyOrder(t *testing.T) {
	in := []string{"Mary Smithers", "Zed Smith", "Ursula K. Le Guin", "Ann Leckie", "Al Smith", "Ann Lee"}
	sort.Slice(in, func(i, j int) bool { return SortKey(in[i]) < SortKey(in[j]) })
	want := []string{"Ann Leckie", "Ann Lee", "Ursula K. Le Guin", "Al Smith", "Zed Smith", "Mary Smithers"}
	if !reflect.DeepEqual(in, want) {
		t.Errorf("order = %q, want %q", in, want)
	}
}

func TestReversed(t *testing.T) {
	cases := []struct {
		in             string
		given, surname string
		ok             bool
	}{
		{"Carroll, Lewis", "Lewis", "Carroll", true},
		{"Le Guin, Ursula K.", "Ursula K.", "Le Guin", true},
		{"de la Cruz, Juana", "Juana", "de la Cruz", true},
		{"Lewis Carroll", "", "", false},
		{"Alexandre Dumas, pere", "", "", false},
		{"Doe, John, Jr.", "", "", false},
		{"Doe,", "", "", false},
	}
	for _, tc := range cases {
		g, s, ok := Reversed(tc.in)
		if g != tc.given || s != tc.surname || ok != tc.ok {
			t.Errorf("Reversed(%q) = %q, %q, %v", tc.in, g, s, ok)
		}
	}
}
