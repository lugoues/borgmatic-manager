package runner

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// An output line beyond the scanner cap must not stop the drain: borgmatic
// would block writing to the full pipe and hold its repo locks forever.
func TestConsumeDrainsAfterOverlongLine(t *testing.T) {
	rs := &runState{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		group:  "g",
	}
	overlong := strings.Repeat("x", 2*1024*1024)
	stream := strings.NewReader(overlong + "\ntrailing output\n")

	done := make(chan struct{})
	go func() {
		rs.consume(stream, "stdout")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consume must drain to EOF after an overlong line, not stop reading")
	}
	if stream.Len() != 0 {
		t.Fatalf("stream not fully drained: %d bytes left", stream.Len())
	}
	if rs.warnings.Load() == 0 {
		t.Fatal("scanner overflow must count as a warning")
	}
}
