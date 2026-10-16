package recorder

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"wcbn-recorder/internal/mpeg"
)

// A resumed recording (the recorder was restarted during the airing, or the
// show was ended and added back) adds its audio to the earlier run's show
// file, after silence as long as the time between them, so the airing is one
// file whose audio keeps time with the clock: songs placed by when they aired
// (see playlistSongs) line up with the audio after the gap too. The file is
// written anew, with the earlier audio, the silence and the new audio, then
// replaces the old one, so it's never left half written.

// minGap and maxGap bound the silence put between runs. A gap shorter than
// minGap is within what the times can tell (the database keeps whole
// seconds, and the audio starts a moment after recording does); a longer one
// than maxGap means the times are wrong (a clock change). Either way the runs
// are joined with none.
const (
	minGap = 2 * time.Second
	maxGap = 24 * time.Hour
)

// mergeShowFile adds the audio in part to the show file at path, after
// silence for the gap, and rewrites its CUE sheet with the earlier tracks and
// this run's (segs).
func (r *recording) mergeShowFile(meta showMeta, part, path string, segs []segment) error {
	old, err := os.Open(path)
	if err != nil {
		return err
	}
	defer old.Close()
	fi, err := old.Stat()
	if err != nil {
		return err
	}
	tag, err := id3Size(old)
	if err != nil {
		return err
	}
	earlier := io.NewSectionReader(old, min(tag, fi.Size()), fi.Size()-min(tag, fi.Size()))
	fw := mpeg.NewFrameWriter(io.Discard)
	if _, err := io.Copy(fw, earlier); err != nil {
		return err
	}
	earlierMs := fw.DurationMs()

	src, err := os.Open(part)
	if err != nil {
		return err
	}
	defer src.Close()
	header := make([]byte, 4)
	if _, err := io.ReadFull(src, header); err != nil {
		return fmt.Errorf("reading the new audio: %w", err)
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	silent, sh, ok := mpeg.SilentFrame(header)
	if !ok {
		return fmt.Errorf("the new audio doesn't start with an MPEG frame")
	}

	// The earlier audio ends when the recording started plus its length, as
	// the database has them, or else when its file was written.
	end := fi.ModTime()
	if e := r.earlier; e.ID != 0 && !e.StartedAt.IsZero() {
		end = e.StartedAt.Add(time.Duration(earlierMs) * time.Millisecond)
	}
	gap := r.started.Sub(end)
	if gap < minGap || gap > maxGap {
		gap = 0
	}
	frames := int64(gap.Seconds() * float64(sh.SampleRate) / float64(sh.Samples))
	silenceMs := frames * int64(sh.Samples) * 1000 / int64(sh.SampleRate)

	if _, err := earlier.Seek(0, io.SeekStart); err != nil {
		return err
	}
	audio := io.MultiReader(earlier, &repeatReader{frame: silent, left: frames}, src)
	prefixMs := earlierMs + silenceMs
	if err := writeTagged(path, r.showTag(meta, prefixMs+r.audio.DurationMs()), audio); err != nil {
		return err
	}
	r.showOut, r.merged = path, true
	r.prefixMs, r.prefixBytes = prefixMs, earlier.Size()+frames*int64(len(silent))
	if gap > 0 {
		log.Printf("[%s] added to %s after %s of silence for the gap", meta.Title, path, gap.Round(time.Second))
	}

	// The CUE sheet: the earlier tracks, then this run's, after the gap.
	tracks := cueTracks(cuePath(path))
	for _, s := range segs {
		title, artist := splitSong(s.mark.title)
		tracks = append(tracks, cueTrack{title, artist, prefixMs + s.startMs})
	}
	if err := writeCueSheet(meta, path, tracks); err != nil {
		log.Printf("writing CUE sheet for %q: %v", meta.Title, err) // the show file itself is fine
	} else {
		r.cueOut = cuePath(path)
	}
	return nil
}

// repeatReader reads frame left times over.
type repeatReader struct {
	frame []byte
	left  int64
	off   int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && r.left > 0 {
		c := copy(p[n:], r.frame[r.off:])
		n += c
		if r.off += c; r.off == len(r.frame) {
			r.off = 0
			r.left--
		}
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// cueTracks reads the tracks of a CUE sheet written by writeCueSheet, or
// none if there isn't one.
func cueTracks(path string) []cueTrack {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var tracks []cueTrack
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, _ := strings.Cut(line, " ")
		switch {
		case key == "TRACK":
			tracks = append(tracks, cueTrack{})
		case len(tracks) == 0: // the sheet's own TITLE and PERFORMER
		case key == "TITLE":
			tracks[len(tracks)-1].title = strings.Trim(value, `"`)
		case key == "PERFORMER":
			tracks[len(tracks)-1].artist = strings.Trim(value, `"`)
		case key == "INDEX" && strings.HasPrefix(value, "01 "):
			tracks[len(tracks)-1].startMs = cueMs(strings.TrimPrefix(value, "01 "))
		}
	}
	return tracks
}

// cueMs parses CUE's MM:SS:FF (75 frames a second) as milliseconds.
func cueMs(s string) int64 {
	var parts [3]int64
	for i, p := range strings.SplitN(s, ":", 3) {
		parts[i], _ = strconv.ParseInt(p, 10, 64)
	}
	return (parts[0]*60+parts[1])*1000 + parts[2]*1000/75
}
