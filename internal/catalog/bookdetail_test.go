package catalog

import "testing"

// Each file on the book page says whether a browser plays it as stored
// (server#410: the console's chapter editor transcodes one that it can't), by
// its own container and codec, a part with no codec of its own by the book's.
func TestAdminFilesDirectPlayable(t *testing.T) {
	t.Parallel()
	b := AdminBook{Path: "A/Book", Codec: "opus"}
	got := adminFiles(b, []BookFile{
		{RelPath: "A/Book/1.mp3", Format: "mp3", Codec: "mp3"},
		{RelPath: "A/Book/2.mka", Format: "mka", Codec: "opus"},
		{RelPath: "A/Book/3.ogg", Format: "ogg"}, // the book's codec: opus, which browsers play
		{RelPath: "A/Book/4.flac", Format: "flac", Codec: "flac"},
		{RelPath: "A/Book/5.m4a", Format: "m4a", Codec: "alac"},
	})
	want := []bool{true, false, true, true, false}
	if len(got) != len(want) {
		t.Fatalf("%d files, want %d", len(got), len(want))
	}
	for i, f := range got {
		if f.DirectPlayable != want[i] {
			t.Errorf("%s direct_playable = %v, want %v", f.Path, f.DirectPlayable, want[i])
		}
	}
}
