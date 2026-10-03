package build

import (
	"bytes"
	"strings"
	"sync"
)

// maxLineLen caps a single log line; minified output can be megabytes long.
const maxLineLen = 4096

// lineWriter is an io.Writer that calls emit once per complete line.
// It is safe for concurrent use because exec.Cmd may write stdout and
// stderr from different goroutines.
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	emit func(string)
}

func newLineWriter(emit func(string)) *lineWriter {
	return &lineWriter{emit: emit}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.line(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxLineLen {
		w.line(w.buf)
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// Flush emits a trailing line that had no newline.
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.line(w.buf)
		w.buf = w.buf[:0]
	}
}

func (w *lineWriter) line(b []byte) {
	s := strings.TrimRight(string(b), "\r")
	if len(s) > maxLineLen {
		s = s[:maxLineLen] + " …(truncated)"
	}
	// Postgres TEXT rejects invalid UTF-8 and NUL bytes; build output may have both.
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
	if s != "" {
		w.emit(s)
	}
}
