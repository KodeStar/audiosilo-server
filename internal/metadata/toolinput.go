package metadata

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

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

// ToolInputBounded is ToolInput for a request's tool run (media's transcode
// on an ffmpeg without the fd: protocol): where it looks at the share (path's
// links resolved and the file opened once more, on Windows) it is a
// LookBounded call (ctx, at most timeout), given up on (GaveUp) on a share
// that stopped answering since the file's open, and what a late answer holds
// let go then; on Unix it looks at nothing there, so it is ToolInput.
func ToolInputBounded(ctx context.Context, f *os.File, root, path string, timeout time.Duration) (input string, extra []*os.File, cleanup func(), err error) {
	if !toolInputLooks {
		return toolInput(f, root, path)
	}
	return boundedToolInput(ctx, path, timeout, func() (string, []*os.File, func(), error) { return toolInput(f, root, path) })
}

// boundedToolInput runs look, a ToolInput on the share at path, as a
// LookBounded call whose late answer, once given up on, is cleaned up (its
// held handle closed) when it arrives.
func boundedToolInput(ctx context.Context, path string, timeout time.Duration, look func() (string, []*os.File, func(), error)) (string, []*os.File, func(), error) {
	type handed struct {
		input   string
		extra   []*os.File
		cleanup func()
	}
	h, err := bounded(ctx, path, timeout, true, func() (handed, error) {
		input, extra, cleanup, err := look()
		return handed{input, extra, cleanup}, err
	}, func(h handed) { h.cleanup() })
	return h.input, h.extra, h.cleanup, err
}

// heldPathInput is ToolInput by path, where a tool inherits no descriptor
// (Windows): path resolved (no link left in it), under root resolved, opened
// there once more and the same file as f. That second handle is held until
// cleanup: os.Open shares reads and writes but not deletion, so while it is
// open the file can't be renamed away or deleted, and no link can take its
// name before the tool opens it (f, opened through an os.Root, shares
// deletion, so it alone would not hold the name). An error when path no
// longer leads under root (errOutsideRoot) or to f (ErrReplacedWhileOpening).
// Built on every platform so its test runs on every one.
func heldPathInput(f *os.File, root, path string) (string, []*os.File, func(), error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, nil, err
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, nil, err
	}
	if _, ok := relBelow(rootReal, real); !ok {
		return "", nil, nil, &os.PathError{Op: "open", Path: path, Err: errOutsideRoot}
	}
	opened, err := f.Stat()
	if err != nil {
		return "", nil, nil, err
	}
	held, err := os.Open(real)
	if err != nil {
		return "", nil, nil, err
	}
	now, err := held.Stat()
	if err == nil && !os.SameFile(opened, now) {
		err = &os.PathError{Op: "open", Path: path, Err: ErrReplacedWhileOpening}
	}
	if err != nil {
		_ = held.Close()
		return "", nil, nil, err
	}
	return real, nil, func() { _ = held.Close() }, nil
}

// InputFormats is the demuxers ffprobe and ffmpeg may read a library file
// with (FFmpeg's -format_whitelist, each by the name ffmpeg -demuxers gives
// it): the containers of the files the server indexes and serves
// (AudioExtensions: MP4 and its family, MP3, Matroska, Ogg with Opus in it,
// FLAC), the plain audio ones a file of theirs may hold under another name
// (WAV, ADTS AAC), and the other audio containers a book may be in under one
// of those names (Wave64, AIFF, WMA's ASF, AC-3, E-AC-3, Monkey's Audio,
// WavPack, CAF, AMR), so such a book probes and plays as it did before the
// list. None of them opens another input (mov's external data references are
// off by default). A file is read by what its bytes are, not by its name, so
// without the list one shaped like a playlist (concat, hls, dash, sdp) under
// an audio name would have the tool open the files or addresses it lists:
// other files on the server.
//
// It is the one list for every run on a library file: a run held to a list
// uses this one, not its own.
const InputFormats = "mov,mp4,m4a,3gp,3g2,mj2,mp3,matroska,webm,ogg,flac,wav,aac,w64,aiff,asf,ac3,eac3,ape,wv,caf,amr"

// InputFormatArgs is the arguments that hold ffprobe or ffmpeg to
// InputFormats for a library file or a podcast episode: before the input for
// ffprobe, before its -i for ffmpeg (an input option). The tool fails
// ("Format not on whitelist") on a file of any other format, nothing it names
// opened. Every run whose demuxer the tool guesses passes them: runFFprobe,
// media.DetectSilences, a transcode and a watch rendition (media's
// transcodeArgs, renditionArgs, on every input they take: fd:, pipe:0 and the
// file protocol, which alone could open what a playlist names), and a new run
// should; the MP3-in-MKA remux names its one demuxer (-f matroska).
func InputFormatArgs() []string { return []string{"-format_whitelist", InputFormats} }

// SweepToolInputs removes the private folders ToolInput made for a tool's
// input on Unix that an earlier run left in the temporary folder (a server
// killed while ffprobe or ffmpeg ran never reached its cleanup), and returns
// how many: only a folder that is one of them by its name, its owner (the
// user the server runs as), its mode and what it holds, and old enough that
// no running tool reads through it. Called once as the server starts.
func SweepToolInputs() int { return sweepToolInputs() }
