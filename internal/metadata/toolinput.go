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
// path now leads (inputLink). On Windows, where no descriptor is inherited so,
// input is path resolved (no link left in it), checked to be under root and
// to be f (os.SameFile) after f was opened: a folder along it can't be
// renamed while a file in it is open there, so it still names f when the tool
// opens it.
func ToolInput(f *os.File, root, path string) (input string, extra []*os.File, cleanup func(), err error) {
	return toolInput(f, root, path)
}
