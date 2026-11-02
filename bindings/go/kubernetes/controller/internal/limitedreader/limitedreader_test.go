package limitedreader

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// readAll reads all bytes from r, returning the data and any non-EOF error.
func readAll(t *testing.T, r io.Reader) ([]byte, error) {
	t.Helper()
	var buf bytes.Buffer
	_, err := io.Copy(&buf, r)
	return buf.Bytes(), err
}

// nopCloser wraps an io.Reader with a no-op Close.
type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

func TestReadCloser_UnderLimit(t *testing.T) {
	data := []byte("hello")
	rc := New(nopCloser{bytes.NewReader(data)}, 10)

	got, err := readAll(t, rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestReadCloser_ExactlyAtLimit(t *testing.T) {
	data := []byte("hello")
	rc := New(nopCloser{bytes.NewReader(data)}, int64(len(data)))

	got, err := readAll(t, rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestReadCloser_OneByteOverLimit(t *testing.T) {
	data := []byte("hello!")
	rc := New(nopCloser{bytes.NewReader(data)}, int64(len(data))-1)

	_, err := readAll(t, rc)
	if err == nil {
		t.Fatal("expected error but got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestReadCloser_MultipleReadsAtLimit(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	rc := New(nopCloser{bytes.NewReader(data)}, int64(len(data)))

	// Read in small chunks
	buf := make([]byte, 7)
	var collected []byte
	for {
		n, err := rc.Read(buf)
		collected = append(collected, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if len(collected) != len(data) {
		t.Fatalf("got %d bytes, want %d", len(collected), len(data))
	}
}
