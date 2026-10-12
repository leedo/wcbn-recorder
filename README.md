# wcbn-recorder

Watches the WCBN program calendar on Spinitron
(<https://spinitron.com/WCBN/calendar>) and records configured shows from the
live stream to disk. Audio is saved exactly as streamed (320 kbps MP3), with
an ID3 tag and per-track chapters added. It's a single Go binary with no
runtime dependencies.

## Usage

```sh
go build -o wcbn-recorder .
cp config.example.json config.json   # edit destination + programs
./wcbn-recorder -config config.json -list    # show upcoming matching shows
./wcbn-recorder -config config.json -test 30s  # record a short clip to verify the setup
./wcbn-recorder -config config.json -shows shows.txt  # list every show with its description
./wcbn-recorder -config config.json          # run continuously
```

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

The sandbox only allows writes under `ReadWritePaths`. If `destination` is
somewhere other than `/var/lib/wcbn-recorder`, add that path to
`ReadWritePaths`. If the destination is under `/home`, also change `ProtectHome`
to `no`. Then make sure the `wcbn-recorder` user can write there.

## Config

| key | default | notes |
|---|---|---|
| `destination` | (required) | root folder for recordings |
| `programs` | (required) | show names, matched case-insensitively against calendar titles (see `-shows`) |
| `match_substring` | `false` | match program names anywhere in the title |
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

## Metadata

An ID3v2.4 tag is written in front of the audio (see `id3.go`). It holds:

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
