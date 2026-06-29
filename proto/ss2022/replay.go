package ss2022proto

import (
	"math"
	"sync"
	"time"
)

const (
	replayWindowSec = 30 // ±30s timestamp tolerance (SIP022)
	saltLifetime    = 60 * time.Second
)

// ReplayGuard rejects requests whose timestamp is outside the ±30s window or whose
// salt has been seen within the last 60s. One guard is shared across all
// server-side connections of a listener.
type ReplayGuard struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

// NewReplayGuard returns an empty guard.
func NewReplayGuard() *ReplayGuard {
	return &ReplayGuard{seen: make(map[string]time.Time), now: time.Now}
}

// checkTime reports whether ts (unix seconds) is within the replay window. A
// garbage far-future ts (> MaxInt64) is rejected rather than wrapping negative.
func (g *ReplayGuard) checkTime(ts uint64) bool {
	if ts > math.MaxInt64 {
		return false
	}
	d := int64(ts) - g.now().Unix()
	if d < 0 {
		d = -d
	}
	return d <= replayWindowSec
}

// checkSalt records salt and reports whether it is fresh (not seen within 60s).
func (g *ReplayGuard) checkSalt(salt []byte) bool {
	key := string(salt)
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for k, t := range g.seen {
		if now.Sub(t) > saltLifetime {
			delete(g.seen, k)
		}
	}
	if t, ok := g.seen[key]; ok && now.Sub(t) <= saltLifetime {
		return false
	}
	g.seen[key] = now
	return true
}
