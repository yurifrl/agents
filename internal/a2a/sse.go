package a2a

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// WriteSSE writes one event as an SSE frame and flushes it.
func WriteSSE(w http.ResponseWriter, flusher http.Flusher, ev Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// SetSSEHeaders prepares a response for server-sent events.
func SetSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

// ReadSSE consumes an SSE stream, calling onEvent for each frame until the
// stream ends, the context is canceled, or a terminal (result/error) event
// arrives. It returns the terminal event.
func ReadSSE(ctx context.Context, body *bufio.Reader, onEvent func(Event)) (Event, error) {
	var last Event
	for {
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		default:
		}

		line, err := body.ReadBytes('\n')
		if err != nil {
			return last, err
		}
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 {
			continue
		}

		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		onEvent(ev)
		last = ev
		if ev.Type == EventResult || ev.Type == EventError {
			return ev, nil
		}
	}
}
