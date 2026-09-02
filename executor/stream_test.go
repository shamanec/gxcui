package executor

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestPrefixWriterTagsWholeLines(t *testing.T) {
	var buf bytes.Buffer
	w := newLineStream(&buf).writer("[batch-01] ")

	// xcodebuild's output arrives in whatever chunks the pipe hands over, so a
	// write is not a line: two here span three lines, one of them split.
	w.Write([]byte("Test Suite started\nTesting App"))
	w.Write([]byte("Tests\n** TEST SUCCEEDED **\n"))

	want := "[batch-01] Test Suite started\n" +
		"[batch-01] Testing AppTests\n" +
		"[batch-01] ** TEST SUCCEEDED **\n"
	if got := buf.String(); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

// A process that ends without a final newline still has something to say.
func TestPrefixWriterFlushesPartialLine(t *testing.T) {
	var buf bytes.Buffer
	w := newLineStream(&buf).writer("[batch-01] ")

	w.Write([]byte("no trailing newline"))
	if buf.Len() != 0 {
		t.Errorf("partial line written before flush: %q", buf.String())
	}

	w.flush()
	if got, want := buf.String(), "[batch-01] no trailing newline\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// A second flush has nothing left to write.
	w.flush()
	if got, want := buf.String(), "[batch-01] no trailing newline\n"; got != want {
		t.Errorf("flush repeated the line: %q", got)
	}
}

// The whole point of the stream: batches run at once, and their lines must not
// land inside one another.
func TestLineStreamKeepsConcurrentLinesWhole(t *testing.T) {
	var buf bytes.Buffer
	stream := newLineStream(&buf)

	const writers, lines = 8, 50

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := stream.writer(fmt.Sprintf("[batch-%02d] ", i))
			for j := range lines {
				// Split each line across two writes, so an unlocked
				// implementation would interleave them.
				w.Write([]byte(fmt.Sprintf("line %d ", j)))
				w.Write([]byte("finished\n"))
			}
		}(i)
	}
	wg.Wait()

	got := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(got) != writers*lines {
		t.Fatalf("got %d lines, want %d", len(got), writers*lines)
	}

	counts := map[string]int{}
	for _, line := range got {
		prefix, rest, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(prefix, "[batch-") {
			t.Fatalf("torn line: %q", line)
		}
		if !strings.HasSuffix(rest, " finished") {
			t.Fatalf("torn line: %q", line)
		}
		counts[prefix]++
	}
	if len(counts) != writers {
		t.Errorf("got lines from %d writers, want %d", len(counts), writers)
	}
	for prefix, n := range counts {
		if n != lines {
			t.Errorf("%s wrote %d lines, want %d", prefix, n, lines)
		}
	}
}

// A nil destination means the caller asked for no streaming at all, and every
// call on the way down has to cope with that.
func TestNilLineStreamProducesNoWriter(t *testing.T) {
	stream := newLineStream(nil)
	if stream != nil {
		t.Fatalf("newLineStream(nil) = %v, want nil", stream)
	}
	if w := stream.writer("[batch-01] "); w != nil {
		t.Errorf("writer() = %v, want nil", w)
	}
	stream.writer("[batch-01] ").flush()
}
