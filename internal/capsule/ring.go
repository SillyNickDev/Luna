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
