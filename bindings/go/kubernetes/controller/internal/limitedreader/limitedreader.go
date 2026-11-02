// Package limitedreader caps reads at a byte limit, returning an error instead of silent
// truncation when the limit is exceeded.
package limitedreader

import (
	"errors"
	"fmt"
	"io"
)

// ReadCloser wraps an io.ReadCloser and caps reads at a byte limit, returning an error
// instead of silently truncating when the underlying stream exceeds the limit.
type ReadCloser struct {
	io.Closer
	limited *io.LimitedReader
}

// New returns a ReadCloser that reads from rc and fails once more than limit bytes are
// available. It takes ownership of rc for Close.
func New(rc io.ReadCloser, limit int64) *ReadCloser {
	return &ReadCloser{Closer: rc, limited: &io.LimitedReader{R: rc, N: limit}}
}

func (l *ReadCloser) Read(p []byte) (int, error) {
	n, err := l.limited.Read(p)
	if err == nil && l.limited.N == 0 {
		// The LimitedReader is exhausted. This could mean the stream is exactly at the
		// limit (fine) or that there is more data beyond it (overflow). Probe one byte
		// from the underlying reader to distinguish the two cases.
		var probe [1]byte
		_, probeErr := l.limited.R.Read(probe[:])
		switch {
		case errors.Is(probeErr, io.EOF):
			return n, nil
		case probeErr == nil:
			return n, fmt.Errorf("resource exceeds maximum allowed size")
		default:
			return n, fmt.Errorf("probe failed while checking resource size: %w", probeErr)
		}
	}
	return n, err
}
