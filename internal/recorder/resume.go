package recorder

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A recording that starts again during the same airing (the recorder was
// restarted, or the show was ended and added back) resumes beside the same
// files. Each recording leaves a hidden marker naming its airing, which
// tells "the same airing again" from "another airing of the show that day".

// markerPath is the airing marker, next to the in-progress file.
func markerPath(out outputs) string {
	return filepath.Join(filepath.Dir(out.part), "."+out.base+".airing")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}

// chooseBase picks the name for a recording's files in the program folder
// dir: that of an earlier recording of the same airing, to resume it, or else
// the first free name, adding suffix (the start time) and then a number for
// other airings on the same day. A folder of that name, from when each song
// was saved as its own file, counts as taken.
func chooseBase(dir, name, suffix, airing string) (base string, resume bool) {
	for i := 0; ; i++ {
		base = name
		switch {
		case i == 1:
			base = name + " " + suffix
		case i > 1:
			base = fmt.Sprintf("%s %s-%d", name, suffix, i)
		}
		out := outputPaths(dir, base)
		marker, err := os.ReadFile(markerPath(out))
		if err == nil && strings.TrimSpace(string(marker)) == airing {
			return base, true
		}
		if !exists(filepath.Join(dir, base)) && !exists(out.showFile) && !exists(markerPath(out)) {
			return base, false
		}
	}
}

// uniquePath returns path, or, if it's taken, path with " 2", " 3", … added
// before ext (e.g. "Show.mp3" becomes "Show 2.mp3").
func uniquePath(path, ext string) string {
	if !exists(path) {
		return path
	}
	stem := strings.TrimSuffix(path, ext)
	for n := 2; ; n++ {
		if p := fmt.Sprintf("%s %d%s", stem, n, ext); !exists(p) {
			return p
		}
	}
}
