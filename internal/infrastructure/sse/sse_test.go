package sse

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestReadDispatchesNamedMultilineAndTrailingEvents(t *testing.T) {
	input := ": keep-alive\r\n" +
		"event: SSE_ACK\r\nid: 1\r\ndata: {\"a\":1}\r\n\r\n" +
		"data: line1\ndata: line2\n\n" +
		"data:no-space\n\n" +
		"event: CHUNK_DELTA\ndata: {\"text\":\"tail\"}" // no trailing blank line
	var events []Event
	if err := Read(strings.NewReader(input), func(event Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []Event{
		{Name: "SSE_ACK", ID: "1", Data: `{"a":1}`},
		{Data: "line1\nline2"},
		{Data: "no-space"},
		{Name: "CHUNK_DELTA", Data: `{"text":"tail"}`},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %#v", events)
	}
	for index := range want {
		if events[index] != want[index] {
			t.Fatalf("event %d = %#v, want %#v", index, events[index], want[index])
		}
	}
}

func TestReadStopsOnHandlerError(t *testing.T) {
	stop := errors.New("stop")
	count := 0
	err := Read(strings.NewReader("data: 1\n\ndata: 2\n\n"), func(Event) error {
		count++
		return stop
	})
	if !errors.Is(err, stop) || count != 1 {
		t.Fatalf("err=%v count=%d", err, count)
	}
}

func TestIdleReaderCancelsWhenStreamStalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pipeReader, pipeWriter := io.Pipe()
	defer pipeWriter.Close()
	reader := NewIdleReader(pipeReader, 50*time.Millisecond, cancel)
	defer reader.Stop()
	go func() { _, _ = pipeWriter.Write([]byte("x")) }()
	buffer := make([]byte, 1)
	if _, err := reader.Read(buffer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("idle reader did not cancel a stalled stream")
	}
}
