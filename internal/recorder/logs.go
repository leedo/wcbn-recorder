package recorder

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// The web UI's Logs page shows the recorder's recent log output. Logs keeps
// the last lines written to the standard logger in memory, and passes them
// on to the terminal (or journald) as before.

// LogLine is one line of log output.
type LogLine struct {
	Seq  int64 // numbers lines from 1, in order
	At   time.Time
	Text string
}

// Logs is an io.Writer for the log package that keeps the last lines written.
type Logs struct {
	out   io.Writer // where log output also goes; nil for nowhere
	stamp bool      // prefix lines written to out with the time

	mu      sync.Mutex
	lines   []LogLine // a ring of up to cap(lines) lines
	start   int       // index of the oldest line, once the ring is full
	seq     int64     // the last line's Seq
	partial []byte    // a line not yet ended by a newline
	changed chan struct{}
}

// NewLogs returns a Logs that keeps the last n lines and copies output to
// out. With stamp, it adds the time to each line written to out, as the log
// package's default flags would; the log package should then add none.
func NewLogs(n int, out io.Writer, stamp bool) *Logs {
	return &Logs{out: out, stamp: stamp, lines: make([]LogLine, 0, max(n, 1)), changed: make(chan struct{})}
}

func (l *Logs) Write(p []byte) (int, error) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	data := append(l.partial, p...)
	l.partial = nil
	var out bytes.Buffer
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			l.partial = append([]byte(nil), data...)
			break
		}
		line := string(data[:i])
		data = data[i+1:]
		l.add(LogLine{At: now, Text: line})
		if l.stamp {
			out.WriteString(now.Format("2006/01/02 15:04:05 "))
		}
		out.WriteString(line + "\n")
	}
	if out.Len() > 0 {
		close(l.changed)
		l.changed = make(chan struct{})
		if l.out != nil {
			l.out.Write(out.Bytes())
		}
	}
	return len(p), nil
}

func (l *Logs) add(line LogLine) {
	l.seq++
	line.Seq = l.seq
	if len(l.lines) < cap(l.lines) {
		l.lines = append(l.lines, line)
		return
	}
	l.lines[l.start] = line
	l.start = (l.start + 1) % len(l.lines)
}

// After returns the lines kept with Seq greater than seq, oldest first, and a
// channel that's closed when another line is written.
func (l *Logs) After(seq int64) ([]LogLine, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []LogLine
	for i := range l.lines {
		if line := l.lines[(l.start+i)%len(l.lines)]; line.Seq > seq {
			out = append(out, line)
		}
	}
	return out, l.changed
}

// Cap is the most lines kept.
func (l *Logs) Cap() int { return cap(l.lines) }
