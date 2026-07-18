package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/tolle-ai/tollecode/internal/alerts"
)

func mountAlerts(r chi.Router) {
	r.Get("/alerts", streamAlerts)
	r.Get("/alerts/history", alertHistory)
	r.Delete("/alerts/{id}", dismissAlert)
}

func streamAlerts(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	var fromOffset int64
	if v := r.URL.Query().Get("from"); v != "" {
		fromOffset, _ = strconv.ParseInt(v, 10, 64)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	// Replay historical alerts from the given offset.
	historical, endOff := alerts.Tail(fromOffset)
	for _, a := range historical {
		writeSSEEvent(w, a)
		flusher.Flush()
	}
	_ = endOff

	// Subscribe for live alerts.
	ch, unsub := alerts.Global.Subscribe()
	defer unsub()

	for {
		select {
		case <-r.Context().Done():
			return
		case a, ok := <-ch:
			if !ok {
				return
			}
			writeSSEEvent(w, a)
			flusher.Flush()
		}
	}
}

func alertHistory(w http.ResponseWriter, r *http.Request) {
	var fromOffset int64
	if v := r.URL.Query().Get("from"); v != "" {
		fromOffset, _ = strconv.ParseInt(v, 10, 64)
	}
	list, _ := alerts.Tail(fromOffset)
	if list == nil {
		list = []alerts.Alert{}
	}
	writeJSON(w, list)
}

func dismissAlert(w http.ResponseWriter, r *http.Request) {
	// Alerts are append-only; dismissal is a client-side concern.
	// Return 204 so clients can call it without error.
	w.WriteHeader(http.StatusNoContent)
}

func writeSSEEvent(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data) //nolint:errcheck
}
