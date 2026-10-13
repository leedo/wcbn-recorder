# wcbn-recorder

Watches the WCBN program calendar on Spinitron
(<https://spinitron.com/WCBN/calendar>) and records the shows you pick from the
live stream to disk. Shows are picked in a web UI, and show details and
selections are kept in a SQLite database. Audio is saved exactly as streamed (320 kbps MP3), with
an ID3 tag and per-track chapters added. It's a single Go binary with no
runtime dependencies.

## Usage

```sh
go build -o wcbn-recorder ./cmd/wcbn-recorder
cp config.example.json config.json   # set destination
./wcbn-recorder -config config.json          # run; then open http://127.0.0.1:8080/
./wcbn-recorder -config config.json -list    # show upcoming selected recordings
./wcbn-recorder -config config.json -test 30s  # record a short clip to verify the setup
./wcbn-recorder -config config.json -shows shows.txt  # list every show with its description
```

## Code layout

```
cmd/wcbn-recorder/    the command: flags and startup
internal/config/      config file loading
internal/httpx/       HTTP client that fills in missing TLS intermediates
internal/spinitron/   Spinitron calendar and show pages
internal/store/       SQLite: show details and selections
internal/mpeg/        MP3 frames and gapless Info frames
internal/id3/         ID3v2.4 tag writer
internal/recorder/    scheduler, recording, song files, web UI (templates/)
```

## Web UI

The main page shows what you're recording: the live panel (while a show is on
the air), the selected shows coming up in the next week, and **Your shows**, the
list of selected shows with their next airing and a **Remove** button.

**+ Add shows** swaps Your shows for the list of every show on the calendar, in
the same place, with its DJ, category, weekly schedule, next airing and
description. Search it or filter it by category, and click **+ Add** to add a
show: you're taken back to Your shows, with the new show highlighted. Shows
already in Your shows are marked **✓ Added**; remove them from Your shows.
**Cancel** goes back without changes. The list opens on its own when nothing is
selected yet, and `/?add=1` links straight to it. Click a column header to sort by it, and click again to reverse. "Schedule"
sorts by weekly time slot, Monday first; "Next airing" sorts by date; "Record"
puts selected shows first. The sort is remembered, and links can set it with
`?sort=<column number>&dir=asc|desc` (columns count from 0).

Selecting a show that is on the air starts recording right away. Removing a show
that is being recorded lets that recording finish.

When nothing is recording, the top of the page shows what's **on air**: the
current program from the schedule, with its artwork, DJ, time slot and progress,
and the song playing now (read from the station's Icecast status page, at most
every 10 seconds). **+ Record this show** selects the program and starts
recording it immediately (it reads **● Record now** if the show is already
selected, for example after ending a recording). **▶ Listen** plays the station
(see Listening below).

While a show is recording, a live panel sits at the top of the page. It shows:

- **status:** REC, RECONNECTING (with the last error), or SAVING while the files
  are written
- **progress:** how far through the time slot the show is, how much has been
  recorded, and when recording stops
- **now playing:** the current song's title and artist, when it started, and how
  long it has played
- **songs recorded so far:** an expandable list in air order, numbered the same
  way as the song files

### Listening

**▶ Listen**, on either panel, plays the station through the recorder at
`/listen`. It's one continuous stream, whatever is or isn't being recorded, so
it keeps playing when a recording starts or ends and when the station moves on
to the next show. **■ Stop listening** disconnects. Because it goes through the
recorder, it works over an SSH tunnel like the rest of the UI, and isn't
affected by the station's incomplete certificate chain.

All listeners share one connection to the station, which the recorder keeps
open for a minute after the last one stops. It holds the last 10 seconds of
audio, so a listener starts 5 seconds behind live and the browser can begin
playing straight away. When a recording is in progress, the connection starts
with the audio just recorded, so listening starts at once. Otherwise the first
listener waits about 5 seconds in Chrome, because the station sends only about
1.6 seconds of audio up front. The station sends every listener the same bytes,
so the recording's audio, and the stream after a dropped connection, are lined
up frame for frame: nothing is repeated or skipped.

**End recording** stops the recording now, after asking to confirm. Everything
recorded so far is saved as usual (whole-show file, songs and playlist), and the
show stays selected, so its next airing is recorded normally. It isn't started
again during the current airing by itself. To record the rest of the airing
after all, remove the show and add it back: recording resumes in the same
folder (see Resuming below).

The panel refreshes every 5 seconds while the page is visible, and right after
you select a show. An expanded song list stays open across refreshes.

Show details come from Spinitron. The calendar is re-read every
`poll_interval`, and each show's page is refetched once a day. **Refresh now**
re-reads both immediately. A selected show that drops off the calendar, for
example between semesters, stays in Your shows and is recorded again when it
returns. Unselected shows that are no longer on the calendar can be removed
from the full list.

The UI has no login, so by default it only listens on `127.0.0.1`. To reach it
from another machine, use an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 server`)
or put it behind a reverse proxy that adds authentication. Avoid simply setting
`listen` to `0.0.0.0:8080`: anyone who can reach the port could change your
selections. Requests from other websites are rejected, so a page open in your
browser can't change them.

## Output

Each recording produces:

```
<destination>/<program>/
  <show> - YYYY-MM-DD/             one file per song, in air order
    .<show> - YYYY-MM-DD.mp3       the whole show, with a chapter per song (hidden backup)
    01 - <artist> - <title>.mp3
    02 - <artist> - <title>.mp3
    …
    <show> - YYYY-MM-DD.m3u8       playlist of the songs
    cover.jpg                      the show's artwork
```

The whole-show file is a backup when song files are on, so it's kept hidden
(a dotfile) inside the song folder, where music libraries such as Plex ignore
it; `ls -a` or your file manager's "show hidden files" reveals it. With
`track_files` off, the whole show is the only output and is written visibly
instead, as `<destination>/<program>/<show> - YYYY-MM-DD.mp3`.

Set `show_file` or `track_files` to `false`
to skip either output. While recording, audio goes to a hidden `.….mp3.part`
file in the show's folder (in the program folder when `track_files` is off).
Each song file is written as soon as the stream moves on to the next song, so
the folder fills up during the show, and the playlist is updated after each
one; only the last song and the whole-show file are written when the show ends.
Ctrl-C / SIGTERM stops gracefully: in-progress recordings are tagged and kept.

### Resuming

If recording of an airing starts again (the recorder was restarted while the
show was on, or the show was ended and added back), it resumes in the same
folder. Song numbering carries on from the last song there, the playlist is
extended, and the song playing at the time of the restart ends up in two files
(before and after), with whatever the recorder missed while it was down. Files
that would otherwise clash get a number before the extension, so the second
whole-show backup is `.<show> - YYYY-MM-DD 2.mp3`. Each recording leaves a
hidden `.<show> - YYYY-MM-DD.airing` marker naming its airing, which is how
the recorder tells a resume from another airing of the same show that day.
Another airing gets its own folder, with the start time added
(`<show> - YYYY-MM-DD 0300`).

The machine has to stay awake: nothing is recorded while it sleeps, and the
recorder reconnects when it wakes. On a laptop, keep it plugged in with sleep
disabled, or run it on an always-on machine (see systemd below).

## Running with systemd (Linux)

`wcbn-recorder.service` runs the recorder as an unprivileged, sandboxed
service:

```sh
GOOS=linux GOARCH=amd64 go build -o wcbn-recorder ./cmd/wcbn-recorder   # or arm64
sudo install -m 755 wcbn-recorder /usr/local/bin/
sudo useradd --system --home-dir /var/lib/wcbn-recorder --shell /usr/sbin/nologin wcbn-recorder
sudo install -d /etc/wcbn-recorder
sudo install -m 644 config.json /etc/wcbn-recorder/   # set "destination", e.g. /var/lib/wcbn-recorder/recordings
sudo install -m 644 wcbn-recorder.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now wcbn-recorder
journalctl -u wcbn-recorder -f
```

The database is created in the service's working directory
(`/var/lib/wcbn-recorder/wcbn-recorder.db`), and the UI listens on
`127.0.0.1:8080` on the server (see the Web UI section for remote access).

The sandbox only allows writes under `ReadWritePaths`. If `destination` is
somewhere other than `/var/lib/wcbn-recorder`, add that path to
`ReadWritePaths`. If the destination is under `/home`, also change `ProtectHome`
to `no`. Then make sure the `wcbn-recorder` user can write there.

## Config

| key | default | notes |
|---|---|---|
| `destination` | (required) | root folder for recordings |
| `show_file` | `true` | write the whole show as one MP3 (hidden in the song folder when `track_files` is on) |
| `track_files` | `true` | write each song as its own MP3, plus a playlist |
| `gapless` | `true` | make song files play back seamlessly (see Songs) |
| `listen` | `127.0.0.1:8080` | address of the web UI |
| `database` | `wcbn-recorder.db` | SQLite file for show details and selections; relative to the working directory |
| `programs` | none | optional show titles to select; imported into the database once, on first start |
| `stream_url` | `https://floyd.wcbn.org:8443/wcbn-hd.mp3` | |
| `schedule_url` | `https://spinitron.com/WCBN/calendar` | Spinitron HTML calendar |
| `poll_interval` | `15m` | how often to re-fetch the schedule |
| `pre_roll` / `post_roll` | `1m` / `2m` | padding around each show |
| `timezone` | `America/Detroit` | for file names and tags |

## Schedule

The calendar page is a fullcalendar widget, so its HTML has no events in it.
The program reads the widget's config from the page to find its event source
(`/WCBN/calendar-feed`), then requests the coming week from that source, as a
browser would. Spinitron splits long shows into back-to-back hourly slots
("Overnight Service", for example), and the program merges those into one
recording. Show descriptions come from each show's Spinitron page. If a refresh
fails, the last good schedule stays in use.

## Audio

Only whole MPEG audio frames are written. Each connection is synced to the
first real frame header, and a partial frame left by a dropped connection is
discarded, so reconnects don't leave corrupt data in the file. The frames
themselves are byte-for-byte what the station sent.

## Songs

Songs are split where the stream's title changes. WCBN's stream titles look
like "<title> by <artist> on <show> with <dj>". The title and artist become the
song file's name and tags, and the last " by " is used to split them, since
song titles ("Stand by Me") contain it more often than artist names. Each song
file is a byte-for-byte slice of the show's audio, cut on MP3 frame boundaries,
so nothing is re-encoded. The playlist is extended M3U, UTF-8 (`.m3u8`), with
durations and relative paths, so the folder can be moved as a unit.

Split points are only as accurate as the stream titles. The title changes when
the DJ logs a song in Spinitron, which can be a few seconds early or late, so a
song can start with the end of the previous one or with the DJ talking. Talk
between songs usually stays with the song before it. Audio before the first
title change (pre-roll) goes into the first file, and if the stream sends no
titles at all, the whole show becomes a single track.

### Gapless playback

Cutting the stream between frames keeps the timing exact, but the first frame
of a song can't be decoded on its own. MP3 frames borrow bits from the frames
before them (the "bit reservoir"; WCBN's stream does this in about 99% of
frames), and the decoder needs a frame of history. Played back to back, plain
song files would have a ~30 ms dropout at the start of every song.

With `gapless` on (the default), each song file starts two frames (52 ms) before
its cut point, and a LAME/Xing "Info" frame at the start of the file tells the
player to skip exactly those samples. The same information is stored as an
`iTunSMPB` comment for Apple players. In players that support gapless MP3, such
as foobar2000, Apple Music, mpv and other ffmpeg-based players, and Plexamp,
consecutive song files then decode to exactly the same samples as the whole
show; the test suite checks this sample for sample. Players without gapless
support play the 52 ms overlap, repeating the end of the previous song.

The first song of a recording, and the first after a reconnect, has no earlier
audio to overlap. Gapless players skip the decoder's own 529-sample (12 ms)
delay at the start of those files.

Each song file's ID3 tag has the title, artist, the show and date as the album,
the DJ as album artist, track number, date, genre, and length. The track number
has no total ("3", not "3/12"), since the number of songs isn't known until the
show ends. A comment says
when it aired. Custom tags hold `show`, `dj`, `aired_at`, `stream_title` (the
raw title from the stream), and `show_url`.

## Artwork

Each show's image from its Spinitron page is embedded as front-cover art in the
whole-show file and every song file, and saved as `cover.jpg` (or `cover.png`)
in the show folder, which Plex and most music apps use as album art. The image
is whatever the show page's `og:image` tag points to, downloaded as is (often a
small Flickr or Spinitron thumbnail). A few shows link Flickr images that no
longer exist; those recordings get the WCBN station logo instead. Only JPEG and PNG images are used, and a
recording goes ahead without art if none can be downloaded.

The web UI shows the same artwork as thumbnails in the live panel, Your shows
and the full show list, loaded by your browser straight from Spinitron and
Flickr (the full list's images only load when you open it and scroll to them).
The artwork URLs are stored with the other show details and refreshed daily.
Broken images fall back to the station logo.

## Metadata

The whole-show file gets an ID3v2.4 tag in front of the audio (see `internal/id3`).
It holds:

- **title:** show name plus date
- **artist:** the DJ, from the calendar, falling back to the stream's
  "… on <show> with <dj>" titles
- **album:** show name
- **album artist** and **publisher:** the station
- **date**
- **genre:** the Spinitron category
- **comment:** air time, show description, and show URL
- **length:** exact duration, counted from the audio frames

Custom tags hold `show`, `dj`, `description`, `show_start`, `show_end`,
`show_url`, and `source`. Every song title the stream announced during the show
becomes a chapter, with a table of contents so players list them in order.
The table of contents can hold at most 255 chapters.
