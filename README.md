# wcbn-recorder

Watches the WCBN program calendar on Spinitron
(<https://spinitron.com/WCBN/calendar>) and records the shows you pick from the
live stream to disk. Shows are picked in a web UI, and show details and
selections are kept in a SQLite database. Audio is saved exactly as streamed (320 kbps MP3), with
an ID3 tag and per-track chapters added. It's a single Go binary with no
runtime dependencies.

## Usage

```sh
go build -o wcbn-recorder .
cp config.example.json config.json   # set destination
./wcbn-recorder -config config.json          # run; then open http://127.0.0.1:8080/
./wcbn-recorder -config config.json -list    # show upcoming selected recordings
./wcbn-recorder -config config.json -test 30s  # record a short clip to verify the setup
./wcbn-recorder -config config.json -shows shows.txt  # list every show with its description
```

## Web UI

The UI lists every show on the calendar with its DJ, category, weekly
schedule, next airing, and description. Click **Record** to select a show, and
use search, the category filter and "Selected only" to narrow the list.
Click a column header to sort by it, and click again to reverse. "Schedule"
sorts by weekly time slot, Monday first; "Next airing" sorts by date; "Record"
puts selected shows first. The sort is remembered, and links can set it with
`?sort=<column number>&dir=asc|desc` (columns count from 0). Above
the list are the recordings in progress and the selected shows coming up in the
next week. Selecting a show that is on the air starts recording right away.
Unselecting a show that is being recorded lets that recording finish.

Show details come from Spinitron. The calendar is re-read every
`poll_interval`, and each show's page is refetched once a day. **Refresh now**
re-reads both immediately. A selected show that drops off the calendar, for
example between semesters, stays in the list and is recorded again when it
returns. Unselected shows that are no longer on the calendar can be removed.

The UI has no login, so by default it only listens on `127.0.0.1`. To reach it
from another machine, use an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 server`)
or put it behind a reverse proxy that adds authentication. Avoid simply setting
`listen` to `0.0.0.0:8080`: anyone who can reach the port could change your
selections. Requests from other websites are rejected, so a page open in your
browser can't change them.

Each recording produces:

```
<destination>/<program>/
  <show> - YYYY-MM-DD.mp3          the whole show, with a chapter per song
  <show> - YYYY-MM-DD/             one file per song, in air order
    01 - <artist> - <title>.mp3
    02 - <artist> - <title>.mp3
    …
    <show> - YYYY-MM-DD.m3u8       playlist of the songs
```

If a recording with that name already exists, the start time is appended
(e.g. `<show> - YYYY-MM-DD 1900`). Set `show_file` or `track_files` to `false`
to skip either output. While recording, audio goes to a hidden `.….part` file,
and the song folder is assembled under a hidden name and renamed into place
when complete. Ctrl-C / SIGTERM stops gracefully: in-progress recordings are
tagged and kept.

## Web UI

The UI lists every show on the calendar with its DJ, category, weekly
schedule, next airing, and description. Click **Record** to select a show, and
use search, the category filter and "Selected only" to narrow the list.
Click a column header to sort by it, and click again to reverse. "Schedule"
sorts by weekly time slot, Monday first; "Next airing" sorts by date; "Record"
puts selected shows first. The sort is remembered, and links can set it with
`?sort=<column number>&dir=asc|desc` (columns count from 0). Above
the list are the recordings in progress and the selected shows coming up in the
next week. Selecting a show that is on the air starts recording right away.
Unselecting a show that is being recorded lets that recording finish.

Show details come from Spinitron. The calendar is re-read every
`poll_interval`, and each show's page is refetched once a day. **Refresh now**
re-reads both immediately. A selected show that drops off the calendar, for
example between semesters, stays in the list and is recorded again when it
returns. Unselected shows that are no longer on the calendar can be removed.

The UI has no login, so by default it only listens on `127.0.0.1`. To reach it
from another machine, use an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 server`)
or put it behind a reverse proxy that adds authentication. Avoid simply setting
`listen` to `0.0.0.0:8080`: anyone who can reach the port could change your
selections. Requests from other websites are rejected, so a page open in your
browser can't change them.

Recordings are written to `<destination>/<program>/<show> - YYYY-MM-DD.mp3`.
If that file already exists, the start time is appended. While recording, audio goes to a hidden
`.….part` file in the same folder. Ctrl-C / SIGTERM stops gracefully:
in-progress recordings are tagged and kept.

## Running with systemd (Linux)

`wcbn-recorder.service` runs the recorder as an unprivileged, sandboxed
service:

```sh
GOOS=linux GOARCH=amd64 go build -o wcbn-recorder .   # or arm64
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
| `show_file` | `true` | write the whole show as one MP3 |
| `track_files` | `true` | write each song as its own MP3, plus a playlist |
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

Each song file's ID3 tag has the title, artist, the show and date as the album,
the DJ as album artist, track number, date, genre, and length. A comment says
when it aired. Custom tags hold `show`, `dj`, `aired_at`, `stream_title` (the
raw title from the stream), and `show_url`.

## Metadata

The whole-show file gets an ID3v2.4 tag in front of the audio (see `id3.go`).
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
