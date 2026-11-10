package base

import (
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
)

// A panic in any goroutine other than a Fiber handler ends the whole
// process (Fiber's recover middleware only covers the request goroutine).
// Background work — crawls, workers, the indexer, live search fan-out —
// runs through these helpers so one bad page or provider costs one run,
// not the server.

// Safe runs fn and turns a panic into an error (logged with its stack).
func Safe(what string, fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			slog.Error("recovered panic", "in", what, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
	return nil
}

// MaxResponseBytes caps any third-party response body read into memory (the
// HTML fetchers use the same 10 MiB): a huge or endless reply must cost one
// failed request, never the container's memory.
const MaxResponseBytes = 10 << 20

// LimitBody wraps a response body for decoding with MaxResponseBytes.
func LimitBody(r io.Reader) io.Reader { return io.LimitReader(r, MaxResponseBytes) }
