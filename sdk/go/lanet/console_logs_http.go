package lanet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Bound simultaneous streams too: each reader owns only one bounded batch.
var consoleLogStreams = make(chan struct{}, 8)

func (c *Client) registerLogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/logs/stream", c.apiLogStream)
	mux.HandleFunc("POST /api/logs/clear", func(w http.ResponseWriter, r *http.Request) {
		processConsoleLogs.clear()
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}

func (c *Client) apiLogStream(w http.ResponseWriter, r *http.Request) {
	cursor, err := strconv.ParseUint(r.URL.Query().Get("cursor"), 10, 64)
	if r.URL.Query().Get("cursor") == "" {
		err = nil
	}
	if err != nil {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}
	generation, err := strconv.ParseUint(r.URL.Query().Get("generation"), 10, 64)
	if r.URL.Query().Get("generation") == "" {
		err = nil
	}
	if err != nil {
		http.Error(w, "invalid generation", http.StatusBadRequest)
		return
	}
	select {
	case consoleLogStreams <- struct{}{}:
		defer func() { <-consoleLogStreams }()
	default:
		http.Error(w, "too many log streams", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := c.mergeCtx(r.Context())
	defer cancel()
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	// Per-write deadlines stop slow/disconnected readers without imposing a
	// lifetime timeout on healthy SSE sessions. Clear the deadline on return.
	defer controller.SetWriteDeadline(time.Time{})
	send := func(payload string) bool {
		if ctx.Err() != nil {
			return false
		}
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprint(w, payload); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !send(": connected\n\n") {
		return
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastHeartbeat := time.Now()
	for {
		batch := processConsoleLogs.batch(cursor, generation)
		if batch.Reset || len(batch.Records) > 0 {
			data, err := json.Marshal(batch)
			if err != nil || !send("event: logs\ndata: "+string(data)+"\n\n") {
				return
			}
			cursor, generation = batch.Cursor, batch.Generation
			lastHeartbeat = time.Now()
		} else if time.Since(lastHeartbeat) >= 15*time.Second {
			if !send(": heartbeat\n\n") {
				return
			}
			lastHeartbeat = time.Now()
		}
		// Always yield between batches; even initial history is never dumped in
		// one response write. Cancellation and Close interrupt this wait.
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
