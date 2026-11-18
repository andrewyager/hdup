package run

import (
	"encoding/json"
	"io"
	"sync"
)

// events writes one JSON object on each line. A nil *events writes nothing.
type events struct {
	mu sync.Mutex
	w  io.Writer
}

func (e *events) emit(v any) {
	if e == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.w.Write(append(b, '\n'))
}

type planEvent struct {
	Event string `json:"event"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

type fileEvent struct {
	Event  string `json:"event"`
	Deck   string `json:"deck"`
	Path   string `json:"path"`
	Key    string `json:"key"`
	Size   *int64 `json:"size,omitempty"`
	Done   *int64 `json:"done,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type summaryEvent struct {
	Event       string  `json:"event"`
	Uploaded    int     `json:"uploaded"`
	Skipped     int     `json:"skipped"`
	Failed      int     `json:"failed"`
	Interrupted int     `json:"interrupted"`
	Bytes       int64   `json:"bytes"`
	Sent        int64   `json:"sent"`
	ElapsedS    float64 `json:"elapsed_s"`
}

type errorEvent struct {
	Event string `json:"event"`
	Error string `json:"error"`
}

// EmitError writes the event for an error that stops the full run.
func EmitError(w io.Writer, err error) {
	(&events{w: w}).emit(errorEvent{Event: "error", Error: err.Error()})
}
