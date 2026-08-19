package main

// logging.go — structured logs that correlate with the traces.
//
// Two channels, deliberately separate:
//
//   - stderr keeps the human startup lines through the stdlib `log` package.
//     Those are for a person watching a terminal and are untouched.
//   - the JSON log file gets one machine-readable line per command, per push
//     and per edit, each stamped with the trace and span id of the span it was
//     emitted under. That is what makes "this log line" and "that span" the
//     same event.
//
// A trace tells you the shape of a request; a summary log line tells you, on
// one row, how long the browser sat in a debounce window before it asked, how
// long the server took, and how big the answer was.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

// obsLog is the structured logger. It defaults to a discarding handler so any
// call made before StartLogging (or with `-log-file=`… well, that goes to
// stderr) is safe.
var obsLog = slog.New(slog.NewJSONHandler(io.Discard, nil))

// traceHandler stamps trace_id/span_id onto every record from the context.
// Doing it in the handler rather than at each call site means a log line can
// never silently lose its correlation because someone forgot an argument.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, rec slog.Record) error {
	if traceID, spanID := spanIDs(ctx); traceID != "" {
		rec.AddAttrs(
			slog.String("trace_id", traceID),
			slog.String("span_id", spanID),
		)
	}
	return h.Handler.Handle(ctx, rec)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}

// StartLogging installs the JSON logger. An empty path logs to stderr, which is
// useful when driving the server by hand but interleaves with the human lines.
func StartLogging(path string) (io.Closer, error) {
	if path == "" {
		obsLog = slog.New(traceHandler{slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})})
		return nopCloser{}, nil
	}
	// Append rather than os.Create, which truncates: a deploy is exactly when
	// the lines from just before it matter most. Rotation is logrotate's job.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("log file: %w", err)
	}
	obsLog = slog.New(traceHandler{slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})})
	return f, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// msf renders a duration as fractional milliseconds. Every duration in the log
// file and every latency attribute on a span uses this unit, so the client's
// performance.now() deltas and the server's own timings can be compared without
// converting anything.
func msf(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}
