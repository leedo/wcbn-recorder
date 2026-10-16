# wcbn-recorder

Watches the WCBN program calendar on Spinitron
(<https://spinitron.com/WCBN/calendar>) and records the shows you pick from the
live stream to disk. Shows are picked in a web UI, and show details and
selections are kept in a SQLite database. Audio is saved exactly as streamed (320 kbps MP3), with
an ID3 tag and a CUE sheet listing the songs. It's a single Go binary with no
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
internal/store/       SQLite: show details, selections, and recorded shows and songs
internal/mpeg/        MP3 frames
internal/id3/         ID3v2.4 tag writer
internal/musicbrainz/ artists' Wikipedia pages, from MusicBrainz
internal/recorder/    scheduler, recording, show files, web UI (templates/)
```

## Web UI

The UI has three pages, **Recordings** (the home page), **Shows** and **Logs**,
with the live panel (what's on air or recording, described below) at the top
of each. Scroll it out of view
and it shrinks into a bar pinned to the top of the window, with the show's
artwork, title and DJ, the current song, a REC badge while recording, and the
Listen button; click the show in the bar to scroll back up to the panel.
Moving between pages doesn't reload the page: the page's script fetches the
next page and swaps it in, so listening carries on. Every page is still an ordinary URL that works on its own.

The UI follows your system's light or dark setting. The sun or moon at the
right end of the footer switches to the other theme, and your browser
remembers the choice; switching back to the system's own theme forgets it,
so the UI follows the system again.
**Recordings** lists the selected shows coming up in the next two days (48
hours) under **Upcoming**, or, if none are, the next one on the schedule; then everything recorded, newest first and ten to a
page, under **Recorded** (see Recordings database below), with a **Play** button for each
finished one. Each links to
its details page, with the show's artwork and everything known about it:
when it aired and was recorded, its length,
size and bitrate, its Spinitron links, its files on disk with their sizes, and
its songs, each with where it starts in the recording, its length and when it
aired; click a song's time to play the recording from it. Artists link to
their Wikipedia pages where one can be found, there and on the live card
(the song playing, or playing back): each artist is looked up on
MusicBrainz (whose own name for the artist has to match), and its Wikidata
entry gives the page. A credit naming several artists ("x/y", "x feat. y") is
looked up whole, then name by name. Lookups run in the background, one
request a second as MusicBrainz asks (the live card's artist as soon as it's
shown), and are kept in the database (an artist
with no page is looked up again after 30 days). The details page of a
recording in progress has an **End recording**
button; the others have **Play** and **Delete**.
Play plays the recording in the live card, in place of the live stream,
showing the song at the current position (from the songs table). Lines on
its progress bar mark where each song starts; hover over one for the song's
title and artist. Click or
use the arrow keys on its progress bar to seek, and **Back to live** to return
to the live card. A resumed airing's show files play as one. Where playback
got to is saved in your browser (its local storage, so each browser keeps its
own), and playing the recording again, or
reloading the page while it's up, resumes from there (from the start once
it's been played to the end). Paused, the URL has `&paused=1`, so a reload
brings the recording back paused rather than playing. **Delete**, after
asking to confirm, deletes the recording's show files and CUE sheets and
removes it from the database. A resumed airing is listed as one recording.
Recordings from when each song was also saved as its own file have a folder
of their own, and Delete removes that folder and everything in it. The list
and details pages update by themselves as recordings start and finish.
**Logs** shows the
recorder's log output: the last 1,000 lines since it started, with new lines
added as they're logged. The log is also written to stderr as usual (with
timestamps, except under journald, which adds its own).

The **Shows** page has two lists. **Your shows** are the shows you're
recording, each with **Remove**. **Add shows** is every other
show on the calendar, with its DJ and category (under the title) and next
airing; search it by show or DJ, or filter it by category, and click **Add**
to record a show. Adding or removing a show moves it to the other list. Old `/?add=1` and `/shows?add=1` links
redirect to the Shows page. Shows no longer on the calendar have a **delete**
link (unless they're selected) to take them off the list. Each show links to
its page, with its artwork, description, schedule and Spinitron link, **Add**
or **Remove**, and its recordings, ten to a page. The show names on the live
card, the upcoming list and a recording's details page link there too.
Both lists start sorted by next airing. Click a column header to sort by
it, and click again to reverse; each list remembers its own sort. "Next
airing" sorts by date. The button column, on the right, isn't sortable.
Links can set Add shows' sort with `?sort=<column number>&dir=asc|desc`
(columns count from 0).

Selecting a show that is on the air starts recording right away. Removing a show
that is being recorded lets that recording finish.

When nothing is recording, the live panel shows what's **on air**: the
current program from the schedule, with its artwork, DJ, time slot and progress,
and the song playing now and the stream's bitrate (read from the station's
Icecast status page, at most every 10 seconds). Adding the show that's on the
air on the Shows page starts recording it immediately.

While a show is recording, the live panel shows the recording instead:

- **status:** REC, RECONNECTING (with the last error), or SAVING while the files
  are written
- **progress:** how far through the time slot the show is, how much has been
  recorded, the stream's bitrate (read from the MP3 frames being saved), and
  when recording stops
- **now playing:** the current song's title and artist

### On a phone

The pages fit a phone's width. On a narrow screen, each show in the Shows
page's lists drops its artwork and puts its next airing under its title, with
Add or Remove beside it, instead of in columns.

To use it like an app on an iPhone, open it in Safari, tap Share, then **Add
to Home Screen**. It opens from its icon (the WCBN logo, named WCBN) in its
own window without Safari's bars, starting on the Recordings page, and the
status bar matches the light or dark theme. Android's **Add to Home screen**
or **Install app** works the same way, using `/manifest.webmanifest`.

### Listening

The **play button** (a ▶ in a circle), on the live panel or the pinned bar, plays the station through the recorder at
`/listen`. It's one continuous stream, whatever is or isn't being recorded, so
it keeps playing when a recording starts or ends and when the station moves on
to the next show. While it plays, the button shows ■ instead, which disconnects. While it's playing, the
URL has `?listen=1`, so reloading the page starts it again, if the browser
lets audio start on its own (otherwise press play). Because it goes through the
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

**End recording**, on the Recordings page, stops the recording now, after asking to confirm. Everything
recorded so far is saved as usual (the show file and CUE sheet), and the
show stays selected, so its next airing is recorded normally. It isn't started
again during the current airing by itself. To record the rest of the airing
after all, remove the show and add it back: recording resumes beside the same
files (see Resuming below).

The server pushes updates to the page (server-sent events, at `/events`) while
it's visible: right away when a recording starts or ends, a show is selected,
or the schedule is refreshed, and every second while recording.

Show details come from Spinitron. The calendar is re-read every
`poll_interval`, and each show's page is refetched once a day. A selected show that drops off the calendar, for
example between semesters, stays selected and is recorded again when it
returns. Unselected shows that are no longer on the calendar can be deleted
from the list.

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
  <show> - YYYY-MM-DD.mp3          the whole show
  <show> - YYYY-MM-DD.cue          CUE sheet for the whole show
```

The CUE sheet lists each song as a track with its title, artist and start
time. Players that support CUE sheets, such as foobar2000, VLC and Kodi, then
show the one file as an album of songs, played gaplessly. The CUE format allows at most 99 tracks, and
minutes go past 99 on long shows, which software players accept.

While recording, audio goes to a hidden `.….mp3.part` file in the program
folder; the show file and CUE sheet are written when the show ends.
Ctrl-C / SIGTERM stops gracefully: in-progress recordings are tagged and kept.

Older versions could also save each song as its own MP3 in a
`<show> - YYYY-MM-DD/` folder, with a `cover.jpg`. Those
recordings stay listed and can still be deleted; the `show_file`,
`track_files` and `gapless` settings are no longer used and are ignored.

### Resuming

If recording of an airing starts again (the recorder was restarted while the
show was on, or the show was ended and added back), it carries on in the same
show file. When the new run finishes, its audio is added to the earlier
run's, after silence as long as the gap between them, so the file keeps time
with the clock: whatever the recorder missed while it was down is silent, and
songs after the gap line up. The silence is MPEG frames like the stream's
with no audio data. The file is written anew and then replaces the old one,
so it's never left half written, and the CUE sheet lists both runs' songs.
(If the audio can't be added, say the new run's doesn't start with an MPEG
frame, the run gets its own show file and CUE sheet instead, with a number
before the extension: `<show> - YYYY-MM-DD 2.mp3`, as older versions always
did; those still play back, and are deleted, along with the first.) Each
recording leaves a hidden `.<show> - YYYY-MM-DD.airing` marker naming its
airing, which is how the recorder tells a resume from another airing of the
same show that day. A resumed airing stays one recording in the database,
with the whole file's size and length.
Another airing gets its own name, with the start time added
(`<show> - YYYY-MM-DD 0300`).

### Recordings database

Each recording is also listed in the database, so past recordings and their
songs can be looked up without reading the folders. The `recordings` table has
a row per recording, with the show's details, the airing's time slot, when
recording started and finished, its size and length, and its files: the
folder, show file and CUE sheet. The `songs` table has a row per song: track
number, title, artist, its album, record label and year (when the DJ logged
them), when it was played, and where it starts in the recording and how long
it is. The details page shows the album, with the label and year in its tooltip.

The songs come from the DJ's playlist on Spinitron, not the stream's titles,
which are often missing or stuck. While a show is recording, its playlist is
read every minute, and the songs logged so far are listed, for DJs who log
them as they play. A finished recording is marked as waiting
for its songs (**finding songs** on the Recordings page) until its playlist
turns up in the calendar feed, which is checked each time the schedule is
refreshed, from 10 minutes after the airing for up to a week. Spinitron gives
each song's time only to the minute, and some playlists are logged with the
wrong clock (hours off), though the gaps between songs are right. So when the
first song's time falls in the show's slot (or up to 15 minutes before), the
times are used as they are; otherwise the first song is put at the start of
the slot and the rest follow at their gaps. The recording's CUE sheet is then
rewritten with the playlist's songs (one per show file, for a resumed airing).

Paths are relative to `destination`, with forward slashes, so the recordings
folder can be moved. A row is added when recording starts, each song as it's
finishes, and the files and length once the show is written; a recording
that gets no audio is removed again. Removing a show
from the list keeps its recordings. Recordings made with `-test` aren't
listed, and a database error is logged without stopping the recording.

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
| `listen` | `127.0.0.1:8080` | address of the web UI |
| `database` | `wcbn-recorder.db` | SQLite file for show details, selections and the list of recordings; relative to the working directory |
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

The stream's titles name the song playing on the live card, and the songs in
the CUE sheet written when the show ends (until the playlist replaces it; see
Recordings database). Songs are split where the stream's title changes. WCBN's stream titles look
like "<title> by <artist> on <show> with <dj>". The title and artist become
the CUE track titles, and the last " by " is used to split them,
since song titles ("Stand by Me") contain it more often than artist names.

Split points are only as accurate as the stream titles. The title changes when
the DJ logs a song in Spinitron, which can be a few seconds early or late, so a
song can start with the end of the previous one or with the DJ talking. Talk
between songs usually stays with the song before it. Audio before the first
title change (pre-roll) goes with the first song, and if the stream sends no
titles at all, the whole show is a single track.

## Artwork

Each show's image from its Spinitron page is embedded as front-cover art in the
show file. The image
is whatever the show page's `og:image` tag points to, downloaded as is (often a
small Flickr or Spinitron thumbnail). A few shows link Flickr images that no
longer exist; those recordings get the WCBN station logo instead. Only JPEG and PNG images are used, and a
recording goes ahead without art if none can be downloaded.

The web UI shows the same artwork as thumbnails in the live panel and the show
list, loaded by your browser straight from Spinitron and Flickr (the list's
images only load as you scroll to them).
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
`show_url`, and `source`.
