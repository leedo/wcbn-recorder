package recorder

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// A CUE sheet next to the whole-show file lists each song as a track of that
// one file, for players that play a single file as an album (foobar2000, VLC,
// Kodi). Playback is gapless, since it's one file.

// maxCueTracks is the most tracks the CUE format allows.
const maxCueTracks = 99

// cuePath is the CUE sheet for a show file: the same name, ending in .cue.
func cuePath(showFile string) string {
	return strings.TrimSuffix(showFile, filepath.Ext(showFile)) + ".cue"
}

// cueTime formats milliseconds as CUE's MM:SS:FF, with 75 frames a second.
// Minutes go past 99 for long shows, which software players accept.
func cueTime(ms int64) string {
	frames := ms * 75 / 1000
	return fmt.Sprintf("%02d:%02d:%02d", frames/75/60, frames/75%60, frames%75)
}

// cueText quotes a value for a CUE sheet, which has no way to escape quotes.
func cueText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return `"` + strings.ReplaceAll(s, `"`, "'") + `"`
}

// cueTrack is a song in a CUE sheet.
type cueTrack struct {
	title, artist string
	startMs       int64 // in the show file
}

// writeCue writes the CUE sheet for the whole-show file at showFile.
func (r *recording) writeCue(meta showMeta, showFile string, segs []segment) error {
	var tracks []cueTrack
	for _, s := range segs {
		title, artist := splitSong(s.mark.title)
		tracks = append(tracks, cueTrack{title, artist, s.startMs})
	}
	return writeCueSheet(meta, showFile, tracks)
}

// writeCueSheet writes the CUE sheet listing tracks for the show file at
// showFile, replacing any there.
func writeCueSheet(meta showMeta, showFile string, tracks []cueTrack) error {
	if len(tracks) > maxCueTracks {
		log.Printf("%q has %d tracks; the CUE sheet lists the first %d", meta.Title, len(tracks), maxCueTracks)
		tracks = tracks[:maxCueTracks]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "REM GENRE %s\n", cueText(meta.Genre))
	fmt.Fprintf(&b, "REM DATE %s\n", meta.date())
	if meta.URL != "" {
		fmt.Fprintf(&b, "REM COMMENT %s\n", cueText(meta.URL))
	}
	fmt.Fprintf(&b, "PERFORMER %s\n", cueText(meta.Artist))
	fmt.Fprintf(&b, "TITLE %s\n", cueText(meta.albumTitle()))
	fmt.Fprintf(&b, "FILE %s MP3\n", cueText(filepath.Base(showFile)))
	for i, t := range tracks {
		fmt.Fprintf(&b, "  TRACK %02d AUDIO\n", i+1)
		fmt.Fprintf(&b, "    TITLE %s\n", cueText(t.title))
		if t.artist != "" {
			fmt.Fprintf(&b, "    PERFORMER %s\n", cueText(t.artist))
		}
		fmt.Fprintf(&b, "    INDEX 01 %s\n", cueTime(t.startMs))
	}
	path := cuePath(showFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
