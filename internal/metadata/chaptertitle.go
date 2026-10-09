package metadata

import (
	"regexp"
	"strings"
)

// ChapterTitle is a chapter's title as the admin console shows it, or "" when the
// title names nothing and the chapter is better called by its place ("Chapter N",
// which the console words in the viewer's language):
//
//   - a filename-shaped title is tidied the way the player's prettifyChapterTitle
//     (audiosilo-frontend src/playback/prettify-title.ts) tidies it: the audio
//     extension dropped, underscores made spaces, a trailing bitrate tag removed;
//   - a title that is empty, only a number ("024", "01."), or a track or disc
//     number ("Track 2-1", "CD1 Track 03": NamesNothing, so a title in another
//     script, "Пролог" or "第1章", stays) is "".
//
// A numbered chapter or part ("Chapter 10", "Part 7") is kept as written: in a
// book that opens with a prologue the 11th chapter is titled "Chapter 10", and
// the player shows that title, so its own number stands.
func ChapterTitle(raw string) string {
	t := strings.TrimSpace(prettifyChapterTitle(raw))
	if NamesNothing(t) && !namesChapter(t) {
		return ""
	}
	return t
}

// namesChapter reports whether a generic title calls itself a chapter or a part
// ("Chapter 10", "Part 07", "chapter12").
func namesChapter(t string) bool {
	for _, f := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if strings.HasPrefix(f, "chapter") || strings.HasPrefix(f, "part") {
			return true
		}
	}
	return false
}

var (
	chapterAudioExt = regexp.MustCompile(`(?i)\.(mp3|m4a|m4b|mp4|aac|ogg|oga|opus|flac|wav|wma|alac|aif|aiff)$`)
	// A trailing encoder bitrate tag left from a rip's filename ("64kb", "128 kbps").
	chapterBitrateTail = regexp.MustCompile(`(?i)(^|\s)\d{2,3}\s?(k|kb|kbps)$`)
)

// prettifyChapterTitle mirrors the player's prettifyChapterTitle: a title with an
// audio extension or underscores is a filename, made readable without inventing
// anything; any other title is returned as it is.
func prettifyChapterTitle(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	if !chapterAudioExt.MatchString(trimmed) && !strings.Contains(trimmed, "_") {
		return raw
	}
	cleaned := chapterAudioExt.ReplaceAllString(trimmed, "")
	cleaned = strings.Join(strings.Fields(strings.ReplaceAll(cleaned, "_", " ")), " ")
	if d := strings.TrimSpace(chapterBitrateTail.ReplaceAllString(cleaned, "")); d != "" {
		cleaned = d
	}
	if cleaned == "" {
		return raw
	}
	return cleaned
}
