// Package sse reads upstream Server-Sent Event streams incrementally so the
// gateway can forward deltas as they arrive instead of buffering whole bodies.
package sse

import (
	"bufio"
	"context"
	"io"
	"strings"
	"sync"
	"time"
)

// Event is one dispatched SSE event. Data joins multi-line data fields with
// "\n" as the SSE specification requires.
type Event struct {
	Name string
	ID   string
	Data string
}

// MaxLineBytes bounds a single SSE line; upstream frames carrying whole
// conversation snapshots can be large but must not grow without limit.
const MaxLineBytes = 16 << 20

// Read parses r and calls handle for every event until EOF, a read error, or
// handle returns an error (which is returned unchanged). A trailing event that
// is not terminated by a blank line is still dispatched at EOF.
func Read(r io.Reader, handle func(Event) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
	var current Event
	var data []string
	hasData := false
	dispatch := func() error {
		if !hasData && current.Name == "" {
			current = Event{}
			return nil
		}
		current.Data = strings.Join(data, "\n")
		event := current
		current, data, hasData = Event{}, data[:0], false
		return handle(event)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keep-alive
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			current.Name = strings.TrimSpace(value)
		case "data":
			data = append(data, value)
			hasData = true
		case "id":
			current.ID = value
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return dispatch()
}

// IdleReader wraps a streaming body and calls cancel when no bytes arrive for
// the configured idle duration. It replaces whole-request timeouts, which cut
// off long reasoning answers that are still actively streaming.
type IdleReader struct {
	reader io.Reader
	idle   time.Duration
	timer  *time.Timer
	once   sync.Once
}

// NewIdleReader starts the idle timer immediately. Call Stop when done.
func NewIdleReader(reader io.Reader, idle time.Duration, cancel context.CancelFunc) *IdleReader {
	result := &IdleReader{reader: reader, idle: idle}
	if idle > 0 {
		result.timer = time.AfterFunc(idle, cancel)
	}
	return result
}

func (r *IdleReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 && r.timer != nil {
		r.timer.Reset(r.idle)
	}
	return n, err
}

// Stop releases the idle timer.
func (r *IdleReader) Stop() {
	r.once.Do(func() {
		if r.timer != nil {
			r.timer.Stop()
		}
	})
}
