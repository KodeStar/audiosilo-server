package metadata

import "os"

// ToolInput is how an external tool (ffprobe, ffmpeg) is handed f, the file
// at path under root opened through root (BelowFolder, OpenRegularBelow), so
// it reads that file and never opens path itself: a tool opening the path
// would follow a link swapped in along it since its check, out of the
// library. input is the tool's input argument, extra the files the command
// passes on (exec.Cmd.ExtraFiles, set as they are: input names the first),
// and cleanup, called once the tool is done, removes what was made for it.
//
// On Unix input is a link in a private folder of its own, named for path's
// extension (as ffmpeg guesses a format by it), to the descriptor the tool
// inherits (/dev/fd/3): the tool opens the file the server opened, wherever
// path now leads (inputLink); with no temporary folder to make it in (a
// read-only one), the descriptor's own name, its format guessed from its
// bytes. On Windows, where no descriptor is inherited so, input is path
// resolved (no link left in it), checked to be under root and, opened once
// more and held until cleanup, to be f (os.SameFile): a folder along it
// can't be renamed while a file in it is open there, nor the file itself
// while that handle (no delete sharing) is, so it still names f when the
// tool opens it.
func ToolInput(f *os.File, root, path string) (input string, extra []*os.File, cleanup func(), err error) {
	return toolInput(f, root, path)
}

// InputFormats is the demuxers ffprobe and ffmpeg may read a library file
// with (FFmpeg's -format_whitelist, each by the name ffmpeg -demuxers gives
// it): the containers of the files the server indexes and serves
// (AudioExtensions: MP4 and its family, MP3, Matroska, Ogg with Opus in it,
// FLAC) and the plain audio ones a file of theirs may hold under another
// name (WAV, ADTS AAC). None of them opens another input. A file is read by
// what its bytes are, not by its name, so without the list one shaped like a
// playlist (concat, hls, dash, sdp) under an audio name would have the tool
// open the files or addresses it lists: other files on the server.
const InputFormats = "mov,mp4,m4a,3gp,3g2,mj2,mp3,matroska,webm,ogg,flac,wav,aac"

// InputFormatArgs is the arguments that hold ffprobe or ffmpeg to
// InputFormats for a library file: before the input for ffprobe, before its
// -i for ffmpeg (an input option). The tool fails ("Format not on
// whitelist") on a file of any other format, nothing it names opened. Every
// run of either on a library file passes them.
func InputFormatArgs() []string { return []string{"-format_whitelist", InputFormats} }
