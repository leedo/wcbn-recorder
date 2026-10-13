package recorder

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wcbn-recorder/internal/store"
)

// addRecording puts a finished recording in the app's database, with its
// files on disk.
func addRecording(t *testing.T, app *App, dir string, files ...string) int64 {
	t.Helper()
	ctx := t.Context()
	at := time.Date(2026, 10, 6, 19, 0, 0, 0, time.UTC)
	id, err := app.store.AddRecording(ctx, store.Recording{Airing: dir, Title: "Train To Skaville", Start: at, End: at.Add(time.Hour), StartedAt: at, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	rec := store.Recording{ID: id, FinishedAt: at.Add(time.Hour)}
	for _, f := range files {
		path := filepath.Join(app.cfg.Destination, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasSuffix(f, ".cue"):
			rec.CueFile = f
		case strings.Contains(filepath.Base(f), "Train To Skaville - "):
			rec.ShowFile = f
		}
	}
	if err := app.store.UpdateRecording(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDeleteRecording(t *testing.T) {
	app, _ := testApp(t)
	app.cfg.Destination = t.TempDir()
	ctx := t.Context()
	dest := app.cfg.Destination
	const dir = "Train To Skaville/Train To Skaville - 2026-10-06"

	// A song folder goes, with every recording in it (here a resumed airing),
	// and then the program folder, now empty.
	first := addRecording(t, app, dir, dir+"/.Train To Skaville - 2026-10-06.mp3", dir+"/01 - Toots - Pressure Drop.mp3", dir+"/cover.jpg")
	addRecording(t, app, dir, dir+"/.Train To Skaville - 2026-10-06 2.mp3")
	other := addRecording(t, app, "Yazoo City Calling/Yazoo City Calling - 2026-10-07", "Yazoo City Calling/Yazoo City Calling - 2026-10-07/01 - x.mp3")
	n, err := app.deleteRecording(ctx, first)
	if err != nil || n != 2 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "Train To Skaville")); !os.IsNotExist(err) {
		t.Errorf("the program folder should be gone: %v", err)
	}
	if recs, _ := app.store.Recordings(ctx); len(recs) != 1 || recs[0].ID != other {
		t.Errorf("left: %+v", recs)
	}
	if _, err := os.Stat(filepath.Join(dest, "Yazoo City Calling/Yazoo City Calling - 2026-10-07/01 - x.mp3")); err != nil {
		t.Errorf("another recording's files were touched: %v", err)
	}

	// With song files off, the folder is the program folder, shared with other
	// recordings: only the recording's own files go.
	solo := addRecording(t, app, "Club H", "Club H/Train To Skaville - 2026-10-06.mp3", "Club H/.Train To Skaville - 2026-10-06.cue")
	keep := filepath.Join(dest, "Club H/Club H - 2026-10-13.mp3")
	os.WriteFile(keep, []byte("x"), 0o644)
	if n, err := app.deleteRecording(ctx, solo); err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dest, "Club H")); len(entries) != 1 || entries[0].Name() != "Club H - 2026-10-13.mp3" {
		t.Errorf("program folder holds %v", entries)
	}

	// Paths from the database must stay inside the destination.
	outside := filepath.Join(filepath.Dir(dest), "outside")
	os.MkdirAll(outside, 0o755)
	t.Cleanup(func() { os.RemoveAll(outside) })
	bad := addRecording(t, app, "../outside")
	if _, err := app.deleteRecording(ctx, bad); err == nil {
		t.Error("deleted a folder outside the destination")
	}
	bad = addRecording(t, app, "Club H", "../outside/Train To Skaville - 2026-10-06.mp3")
	if _, err := app.deleteRecording(ctx, bad); err == nil {
		t.Error("deleted a file outside the destination")
	}
	if _, err := os.Stat(filepath.Join(outside, "Train To Skaville - 2026-10-06.mp3")); err != nil {
		t.Errorf("outside file: %v", err)
	}

	// A recording in progress can't be deleted, nor can a folder one is
	// still writing to.
	f := newFakeRecording(t, testConfig())
	f.cfg.Destination = dest
	f.catalogID = other
	app.mu.Lock()
	app.active["x"] = f.recording
	app.mu.Unlock()
	if _, err := app.deleteRecording(ctx, other); !errors.Is(err, errInProgress) {
		t.Errorf("deleting a recording in progress: %v", err)
	}
	f.catalogID = 999
	f.out = outputPaths(f.cfg, filepath.Join(dest, "Yazoo City Calling"), "Yazoo City Calling - 2026-10-07")
	if _, err := app.deleteRecording(ctx, other); !errors.Is(err, errInProgress) {
		t.Errorf("deleting a folder being recorded into: %v", err)
	}
}

// The Recordings page has End recording for recordings in progress and
// Delete for the rest, and both work.
func TestWebRecordingActions(t *testing.T) {
	app, srv := testApp(t)
	app.cfg.Destination = t.TempDir()
	const dir = "Train To Skaville/Train To Skaville - 2026-10-06"
	done := addRecording(t, app, dir, dir+"/01 - x.mp3")

	f := newFakeRecording(t, testConfig())
	f.frames(1, 3)
	live, err := app.store.AddRecording(t.Context(), store.Recording{Airing: f.job.Event.Key(), Title: f.job.Event.Title, Start: f.job.Event.Start, End: f.job.Event.End, StartedAt: time.Now(), Dir: "Train To Skaville/Train To Skaville - 2026-10-13"})
	if err != nil {
		t.Fatal(err)
	}
	f.catalogID = live
	app.mu.Lock()
	app.active[f.job.Event.Key()] = f.recording
	app.mu.Unlock()

	page := get(t, srv.URL+"/recordings")
	for _, want := range []string{
		fmt.Sprintf(`action="/recordings/%d/stop" class="stop-form"`, live),
		fmt.Sprintf(`action="/recordings/%d/delete" class="rec-delete-form"`, done),
		`data-own-folder="true"`, fmt.Sprintf(`data-state="%d:false"`, live),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("recordings page missing %q", want)
		}
	}
	if strings.Contains(page, fmt.Sprintf("/recordings/%d/delete", live)) || strings.Contains(page, fmt.Sprintf("/recordings/%d/stop", done)) {
		t.Error("only finished recordings can be deleted, and only ones in progress ended")
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, form url.Values) *http.Response {
		t.Helper()
		resp, err := client.PostForm(srv.URL+path, form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := post(fmt.Sprintf("/recordings/%d/delete", live), nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("deleting a recording in progress: %s", resp.Status)
	}
	if resp := post(fmt.Sprintf("/recordings/%d/delete", done), nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/recordings" {
		t.Errorf("delete: %s %s", resp.Status, resp.Header.Get("Location"))
	}
	if _, err := os.Stat(filepath.Join(app.cfg.Destination, dir)); !os.IsNotExist(err) {
		t.Errorf("folder not deleted: %v", err)
	}
	if resp := post(fmt.Sprintf("/recordings/%d/delete", done), nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleting it again: %s", resp.Status)
	}
	if resp := post(fmt.Sprintf("/recordings/%d/stop", live), url.Values{"from": {"recordings"}}); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/recordings" {
		t.Errorf("end: %s %s", resp.Status, resp.Header.Get("Location"))
	}
	if page := get(t, srv.URL+"/recordings"); !strings.Contains(page, `<span class="tag">saving</span>`) || !strings.Contains(page, fmt.Sprintf(`data-state="%d:true"`, live)) {
		t.Error("an ended recording should show as saving, with no buttons")
	}
}
