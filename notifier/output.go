package notifier

import (
	"bytes"
	"sync"
)

const maxResponseBytes = 64 << 10

// limitedOutput drains command output while bounding retained log data.
// SSH copies stdout and stderr concurrently.
type limitedOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if remaining := maxResponseBytes - b.buf.Len(); len(p) > remaining {
		p = p[:remaining]
	}
	_, _ = b.buf.Write(p)
	return n, nil
}

func (b *limitedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
