package recorder

import "time"

// liveStatus is a snapshot of a recording in progress, for the web UI.
type liveStatus struct {
	ID         int64 // the recording's row in the recordings table, for End recording; 0 if it isn't in the database
	Key        string
	ShowID     int64
	Title      string
	DJ         string
	Image      string    // artwork URL, from the database
	Start, End time.Time // scheduled slot
	Until      time.Time // recording stops, including post-roll
	Started    time.Time // recording actually began
	Progress   int       // percent of the scheduled slot elapsed
	RecordedMs int64
	Bytes      int64
	Bitrate    int // kbps, from the stream's frames
	Connected  bool
	Finishing  bool // writing output files
	Stopped    bool // ended early from the UI
	LastErr    string
	Reconnects int
	SongFiles  bool // song files are being written
	Saved      int  // songs saved as files so far
	Current    *liveTrack
	Tracks     []liveTrack // air order
}

type liveTrack struct {
	N          int
	Title      string
	Artist     string
	At         time.Time
	DurationMs int64
	Current    bool
	Saved      bool // written to its own file
}

func (r *recording) status(now time.Time) liveStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := r.job.Event
	st := liveStatus{
		ID: r.catalogID, Key: ev.Key(), Title: ev.Title, DJ: ev.DJ,
		Start: ev.Start, End: ev.End, Until: r.job.Until, Started: r.started,
		RecordedMs: r.audio.DurationMs(), Bytes: r.audio.Written(), Bitrate: r.audio.Bitrate(),
		Connected: r.connected, Finishing: r.finishing, Stopped: r.stopped, LastErr: r.lastErr, Reconnects: r.reconnects,
		SongFiles: r.cfg.TrackFiles, Saved: len(r.songsSaved),
	}
	if st.DJ == "" {
		st.DJ = r.dj
	}
	if slot := ev.End.Sub(ev.Start); slot > 0 {
		st.Progress = int(min(max(now.Sub(ev.Start)*100/slot, 0), 100))
	}
	// Number songs the way song files are numbered: titles that never got any audio
	// (replaced before the next chunk arrived) are skipped, except the
	// current one, which is still growing.
	for i, m := range r.tracks {
		end := st.RecordedMs
		last := i == len(r.tracks)-1
		if !last {
			end = r.tracks[i+1].ms
			if r.tracks[i+1].offset == m.offset {
				continue
			}
		}
		title, artist := splitSong(m.title)
		st.Tracks = append(st.Tracks, liveTrack{
			N: r.songBase + len(st.Tracks) + 1, Title: title, Artist: artist, At: m.at,
			DurationMs: end - m.ms, Current: last, Saved: r.songsSaved[r.songBase+len(st.Tracks)+1],
		})
	}
	if n := len(st.Tracks); n > 0 {
		st.Current = &st.Tracks[n-1]
	}
	return st
}
