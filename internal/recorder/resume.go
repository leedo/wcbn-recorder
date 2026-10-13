package recorder

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"wcbn-recorder/internal/config"
)

// A recording that starts again during the same airing (the recorder was
// restarted, or the show was ended and added back) resumes in the same files
// and folder. Each recording leaves a hidden marker naming its airing, which
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
// dir: the folder of an earlier recording of the same airing, to resume it,
// or else the first free name, adding suffix (the start time) and then a
// number for other airings on the same day.
func chooseBase(cfg *config.Config, dir, name, suffix, airing string) (base string, resume bool) {
	for i := 0; ; i++ {
		base = name
		switch {
		case i == 1:
			base = name + " " + suffix
		case i > 1:
			base = fmt.Sprintf("%s %s-%d", name, suffix, i)
		}
		out := outputPaths(cfg, dir, base)
		marker, err := os.ReadFile(markerPath(out))
		if err == nil && strings.TrimSpace(string(marker)) == airing {
			return base, true
		}
		if !exists(out.trackDir) && !exists(filepath.Join(dir, base+".mp3")) && !exists(markerPath(out)) {
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

var songFileRe = regexp.MustCompile(`^(\d+) - .*\.mp3$`)

// lastSongNumber is the highest song number already in a show folder, so a
// resumed recording carries on from there.
func lastSongNumber(dir string) int {
	entries, _ := os.ReadDir(dir)
	last := 0
	for _, e := range entries {
		if m := songFileRe.FindStringSubmatch(e.Name()); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > last {
				last = n
			}
		}
	}
	return last
}

// playlistEntries reads the songs (EXTINF and file lines) from an existing
// playlist, so a resumed recording adds to it.
func playlistEntries(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var entries []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line == "#EXTM3U" || strings.HasPrefix(line, "#PLAYLIST:") {
			continue
		}
		entries = append(entries, line)
	}
	return entries
}
