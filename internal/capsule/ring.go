package capsule

import (
	"strings"
	"sync"
)

// Ring keeps a capsule's most recent log output, bounded by bytes.
type Ring struct {
	mu      sync.Mutex
	max     int
	size    int
	lines   []string
	partial string
	total   int // lines ever written, so a follower can ask for what's new
}

func NewRing(max int) *Ring { return &Ring{max: max} }

// Write takes stdout, stderr and log frames. Lines are capped at 4 KiB.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.partial + string(p)
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 && len(s) < 4096 {
			break
		}
		if i < 0 || i > 4096 {
			i = 4096
			r.add(s[:i])
			s = s[i:]
			continue
		}
		r.add(s[:i])
		s = s[i+1:]
	}
	r.partial = s
	return len(p), nil
}

func (r *Ring) add(l string) {
	r.total++
	r.lines = append(r.lines, l)
	r.size += len(l)
	for r.size > r.max {
		r.size -= len(r.lines[0])
		r.lines = r.lines[1:]
	}
}

// Tail returns the last n lines.
func (r *Ring) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines[max(0, len(r.lines)-n):]...)
}

// Since returns lines written after the first n ever written (as far as the ring still
// holds them) and the new total.
func (r *Ring) Since(n int) ([]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > r.total {
		n = 0 // a new ring (the capsule restarted): everything is new
	}
	start := len(r.lines) - (r.total - n)
	if start < 0 {
		start = 0
	}
	if start > len(r.lines) {
		start = len(r.lines)
	}
	return append([]string(nil), r.lines[start:]...), r.total
}
