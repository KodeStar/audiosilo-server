package meta

import "testing"

func TestDerivePathFacts(t *testing.T) {
	for _, tc := range []struct {
		path     string
		isFolder bool
		want     pathFacts
	}{
		// The leaf goes up as named: metaserve reads its numbering.
		{
			path: "Bernard Cornwell/Richard Sharpe/Sharpe - 08 - Sharpe's Eagle", isFolder: true,
			want: pathFacts{Author: "Bernard Cornwell", Series: "Richard Sharpe", Title: "Sharpe - 08 - Sharpe's Eagle"},
		},
		{
			path: "J.N. Chaney/Sentenced to War/SW06 - An Alliance Reformed", isFolder: true,
			want: pathFacts{Author: "J.N. Chaney", Series: "Sentenced to War", Title: "SW06 - An Alliance Reformed"},
		},
		{
			// A single file: the extension is not part of the title.
			path: "Will Wight/Cradle/01 - Unsouled.m4b",
			want: pathFacts{Author: "Will Wight", Series: "Cradle", Title: "01 - Unsouled"},
		},
		{
			// One folder deep: the folder is the author, not a series.
			path: "George Orwell/1984", isFolder: true,
			want: pathFacts{Author: "George Orwell", Title: "1984"},
		},
		{
			// Disc folders are parts of the book above them.
			path: "Cressida Cowell/06 - A Hero's Guide to Deadly Dragons/CD1", isFolder: true,
			want: pathFacts{Author: "Cressida Cowell", Title: "06 - A Hero's Guide to Deadly Dragons"},
		},
		{
			path: "Cressida Cowell/How to Be a Pirate/Disc 2/Track 01.mp3",
			want: pathFacts{Author: "Cressida Cowell", Title: "How to Be a Pirate"},
		},
		{
			// A sub-series folder goes up as named too ("03 - Tawny Man").
			path: "Robin Hobb/Realms of the Elderlings/03 - Tawny Man/TM02 - Golden Fool", isFolder: true,
			want: pathFacts{Author: "Robin Hobb", Series: "03 - Tawny Man", Title: "TM02 - Golden Fool"},
		},
		// Root files carry no hierarchy at all.
		{path: "01 - Unsouled.m4b", want: pathFacts{Title: "01 - Unsouled"}},
		{path: "Dune.m4b", want: pathFacts{Title: "Dune"}},
		{path: "Track 01.mp3", want: pathFacts{}},
		// A bare-number leaf under a holding folder is a volume of the series
		// that folder names, not a part of a book it names.
		{
			path: "Brandon Sanderson/Stormlight Archive/03", isFolder: true,
			want: pathFacts{Author: "Brandon Sanderson", Series: "Stormlight Archive", Position: "3"},
		},
		{
			path: "Brandon Sanderson/Stormlight Archive/03.m4b",
			want: pathFacts{Author: "Brandon Sanderson", Series: "Stormlight Archive", Position: "3"},
		},
		{
			path: "Author/Saga/Sub Series/12/Track 01.mp3",
			want: pathFacts{Author: "Author", Series: "Sub Series", Position: "12"},
		},
		// ... but a disc label stays a part of the book above it.
		{path: "Some Author/Some Book/CD1", isFolder: true, want: pathFacts{Author: "Some Author", Title: "Some Book"}},
		{path: "Some Author/Some Book/Part 3", isFolder: true, want: pathFacts{Author: "Some Author", Title: "Some Book"}},
		{path: "Some Author/Some Book/CD1/03", isFolder: true, want: pathFacts{Author: "Some Author", Title: "Some Book"}},
		{path: "", isFolder: true, want: pathFacts{}},
		// A bare number under the author is never dropped down to the author
		// folder alone: the author stays the author.
		{path: "Peter Clines/14", isFolder: true, want: pathFacts{Author: "Peter Clines"}},
		{path: "Stephen King/11.22.63", isFolder: true, want: pathFacts{Author: "Stephen King"}},
		// Names in other scripts are names, not empty part labels.
		{path: "Лев Толстой/Война и мир", isFolder: true, want: pathFacts{Author: "Лев Толстой", Title: "Война и мир"}},
		{path: "村上春樹/ノルウェイの森.m4b", want: pathFacts{Author: "村上春樹", Title: "ノルウェイの森"}},
		// ... numbered ones too: the number is not all the name says.
		{
			path: "Дмитрий Глуховский/Метро/Метро 2033", isFolder: true,
			want: pathFacts{Author: "Дмитрий Глуховский", Series: "Метро", Title: "Метро 2033"},
		},
		{path: "刘慈欣/三体 2", isFolder: true, want: pathFacts{Author: "刘慈欣", Title: "三体 2"}},
	} {
		if got := derivePathFacts(tc.path, tc.isFolder); got != tc.want {
			t.Errorf("derivePathFacts(%q) = %+v, want %+v", tc.path, got, tc.want)
		}
	}
}
