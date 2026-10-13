package recorder

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLogs(t *testing.T) {
	var out bytes.Buffer
	logs := NewLogs(3, &out, true)
	lg := log.New(logs, "", 0)
	for i := 1; i <= 4; i++ {
		lg.Printf("line %d", i)
	}
	// A line can arrive in pieces, and one write can hold several lines.
	logs.Write([]byte("part"))
	logs.Write([]byte("ial\nsix\n"))

	lines, changed := logs.After(0)
	var texts []string
	for _, l := range lines {
		texts = append(texts, fmt.Sprintf("%d %s", l.Seq, l.Text))
	}
	if got := strings.Join(texts, ", "); got != "4 line 4, 5 partial, 6 six" {
		t.Errorf("kept %q", got)
	}
	if after, _ := logs.After(5); len(after) != 1 || after[0].Text != "six" {
		t.Errorf("after 5: %+v", after)
	}
	// Output is passed on, with the time.
	if !regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d line 1\n`).Match(out.Bytes()) || strings.Count(out.String(), "\n") != 6 {
		t.Errorf("output:\n%s", out.String())
	}
	select {
	case <-changed:
		t.Error("changed was closed before another line")
	default:
	}
	lg.Print("seven")
	select {
	case <-changed:
	default:
		t.Error("changed wasn't closed by a new line")
	}
}

func TestWebLogs(t *testing.T) {
	app, srv := testApp(t)
	lg := log.New(app.logs, "", 0)
	lg.Print("recording <b>Club H</b>")

	page := get(t, srv.URL+"/logs")
	for _, want := range []string{`<main id="page" data-page="logs"`, `<a href="/logs" aria-current="page">Logs</a>`, "<title>Logs · WCBN Recorder</title>",
		`<span>recording &lt;b&gt;Club H&lt;/b&gt;</span>`, `data-after="1"`, "The last 100 lines"} {
		if !strings.Contains(page, want) {
			t.Errorf("logs page missing %q", want)
		}
	}

	// The event stream sends lines logged after the ones on the page.
	resp, err := http.Get(srv.URL + "/logs/events?after=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(50 * time.Millisecond)
		lg.Print("saved song 1")
	}()
	sc := bufio.NewScanner(resp.Body)
	var id string
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "id: "); ok {
			id = v
		}
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var msg struct{ HTML string }
			json.Unmarshal([]byte(data), &msg)
			if id != "2" || !strings.Contains(msg.HTML, "<span>saved song 1</span>") || strings.Contains(msg.HTML, "Club H") {
				t.Errorf("event %s: %s", id, msg.HTML)
			}
			return
		}
	}
	t.Fatalf("no event: %v", sc.Err())
}
