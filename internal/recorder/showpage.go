package recorder

import (
	"fmt"
	"net/http"
	"strconv"
)

// A show's page (/shows/{id}) has its details from Spinitron (artwork,
// description, schedule), Add or Remove, and its recordings, listed like the
// Recordings page's, ten to a page.

type showDetails struct {
	showView
	StationLogo string
	Recordings  []recordingView
	Pager       pager
	State       string // see recordingsState
}

// pager links the pages of a list of recordings: Path?page=N#Anchor.
type pager struct {
	Path, Anchor string
	Page, Pages  int // Page counts from 1 (the most recent); Pages is at least 1
}

// URL is the address of page n.
func (p pager) URL(n int) string {
	if n <= 1 {
		return p.Path + "#" + p.Anchor
	}
	return fmt.Sprintf("%s?page=%d#%s", p.Path, n, p.Anchor)
}

// recordingsList is the page of the recordings of the show with ID showID
// (or of every show, for 0) asked for with ?page=N, recordingsPerPage at a
// time, newest first; a page past the end shows the last. active is the
// recordings in progress; path and anchor are for the pager's links.
func (a *App) recordingsList(r *http.Request, showID int64, active []liveStatus, path, anchor string) ([]recordingView, pager, error) {
	p := pager{Path: path, Anchor: anchor}
	p.Page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	p.Page = max(p.Page, 1)
	recs, total, err := a.store.RecordingsPage(r.Context(), showID, recordingsPerPage, (p.Page-1)*recordingsPerPage)
	if err != nil {
		return nil, p, err
	}
	p.Pages = max((total+recordingsPerPage-1)/recordingsPerPage, 1)
	if p.Page > p.Pages {
		p.Page = p.Pages
		if recs, _, err = a.store.RecordingsPage(r.Context(), showID, recordingsPerPage, (p.Page-1)*recordingsPerPage); err != nil {
			return nil, p, err
		}
	}
	byID := map[int64]liveStatus{}
	for _, st := range active {
		byID[st.ID] = st
	}
	var views []recordingView
	for _, rec := range recs {
		st, inProgress := byID[rec.ID]
		views = append(views, recordingView{
			Recording: rec, InProgress: inProgress, Ending: st.Finishing || st.Stopped, OwnFolder: ownsFolder(rec.Dir),
		})
	}
	return views, p, nil
}

// showPage handles /shows/{id}.
func (a *App) showPage(w http.ResponseWriter, r *http.Request) (layoutData, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return layoutData{}, false
	}
	data, err := a.pageData(r.Context())
	if err != nil {
		httpError(w, err)
		return layoutData{}, false
	}
	d := showDetails{StationLogo: data.StationLogo, State: recordingsState(data.Recording, data.SongsChanged)}
	found := false
	for _, sh := range data.Shows {
		if sh.ID == id {
			d.showView, found = sh, true
		}
	}
	if !found {
		http.NotFound(w, r)
		return layoutData{}, false
	}
	if d.Recordings, d.Pager, err = a.recordingsList(r, id, data.Recording, fmt.Sprintf("/shows/%d", id), "recorded"); err != nil {
		httpError(w, err)
		return layoutData{}, false
	}
	return layoutData{Page: "show", Title: d.Title, Live: data, Data: d}, true
}
