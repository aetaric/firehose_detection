package main

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
)

// Detection is one record (post, profile, ...) that tripped at least one
// rule. It is the record handed to the reporting pipeline.
type Detection struct {
	URI        string    `json:"uri"`
	CID        string    `json:"cid,omitempty"`
	DID        string    `json:"did"`
	Collection string    `json:"collection"` // record NSID, e.g. app.bsky.actor.profile
	Operation  string    `json:"operation"`
	Seq        uint64    `json:"seq"`
	TimeUS     int64     `json:"time_us"`
	CreatedAt  string    `json:"created_at,omitempty"`
	Text       string    `json:"text"`
	Media      string    `json:"media,omitempty"` // e.g. "2 images"; the media itself is never included
	Hits       []RuleHit `json:"hits"`
}

// Sink receives detections. Implementations decide where they go (stdout,
// a queue, a webhook, ...). Flush is called once per firehose batch, before
// the cursor is persisted, so a sink that buffers must make its writes
// durable there.
type Sink interface {
	Emit(Detection) error
	Flush() error
}

// JSONLSink writes one JSON object per line. Point it at stdout to pipe into
// jq, vector, fluent-bit, a kafka producer, etc.
type JSONLSink struct {
	mu  sync.Mutex
	w   *bufio.Writer
	enc *json.Encoder
}

func NewJSONLSink(w io.Writer) *JSONLSink {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	return &JSONLSink{w: bw, enc: enc}
}

func (s *JSONLSink) Emit(d Detection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(d)
}

func (s *JSONLSink) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Flush()
}

// MultiSink fans detections out to several sinks.
type MultiSink []Sink

func (m MultiSink) Emit(d Detection) error {
	for _, s := range m {
		if err := s.Emit(d); err != nil {
			return err
		}
	}
	return nil
}

func (m MultiSink) Flush() error {
	for _, s := range m {
		if err := s.Flush(); err != nil {
			return err
		}
	}
	return nil
}
