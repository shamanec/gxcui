package executor

import (
	"bytes"
	"io"
	"sync"
)

// lineStream fans several concurrent producers onto one destination, a whole
// line at a time.
//
// Streaming xcodebuild's output is only worth doing if it stays readable, and
// batches run on several simulators at once: written straight through, one
// batch's lines would land in the middle of another's. Each writer buffers
// until it has a complete line, then takes the lock and emits it with a tag
// saying which batch it came from.
type lineStream struct {
	mu sync.Mutex
	w  io.Writer
}

// newLineStream returns a stream writing to w, or nil when w is nil. A nil
// stream is usable: its writers are nil, and callers skip them.
func newLineStream(w io.Writer) *lineStream {
	if w == nil {
		return nil
	}
	return &lineStream{w: w}
}

// writer returns a writer that tags every line with prefix, or nil when the
// stream is nil. The result is safe to use from several goroutines at once,
// which matters because a process' stdout and stderr arrive on two.
func (s *lineStream) writer(prefix string) *prefixWriter {
	if s == nil {
		return nil
	}
	return &prefixWriter{stream: s, prefix: prefix}
}

// prefixWriter is one producer's end of a lineStream.
type prefixWriter struct {
	stream *lineStream
	prefix string
	buf    bytes.Buffer
}

var _ io.Writer = (*prefixWriter)(nil)

// Write buffers p and emits every complete line it now holds.
//
// It never reports an error: this is diagnostic output, and a closed pipe on
// the far end is no reason to fail a test run that is otherwise fine.
func (w *prefixWriter) Write(p []byte) (int, error) {
	w.stream.mu.Lock()
	defer w.stream.mu.Unlock()

	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// Not a whole line yet. ReadString consumed it either way, so put
			// the remainder back and wait for the rest to arrive.
			w.buf.Reset()
			w.buf.WriteString(line)
			break
		}
		w.emit(line)
	}
	return len(p), nil
}

// flush emits a trailing partial line, which is how a process that ends
// without a final newline would otherwise be lost.
func (w *prefixWriter) flush() {
	if w == nil {
		return
	}
	w.stream.mu.Lock()
	defer w.stream.mu.Unlock()

	if w.buf.Len() == 0 {
		return
	}
	w.emit(w.buf.String() + "\n")
	w.buf.Reset()
}

// emit writes one newline-terminated line. The caller holds the lock.
func (w *prefixWriter) emit(line string) {
	if w.prefix != "" {
		io.WriteString(w.stream.w, w.prefix)
	}
	io.WriteString(w.stream.w, line)
}
