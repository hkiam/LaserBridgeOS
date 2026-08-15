package api

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// One password guards everything this appliance protects: the SSH login, root
// through doas, and the authorisation to install firmware. Two endpoints check
// it, and both are reachable by anyone who can reach port 80 - the web
// interface has no login of its own (ADR 0014), which is a deliberate choice
// for a workshop network and not a reason to make guessing cheap.
//
// What used to stand in the way was a one-second sleep on a wrong password.
// That is not a rate limit. It costs one server goroutine per attempt, it does
// nothing at all against attempts made in parallel, and a second per guess is
// still tens of thousands of guesses a day.
//
// This is shared between the endpoints on purpose: they check the same secret,
// so counting them separately would just mean guessing on whichever one is
// currently cheap.
type attemptGuard struct {
	mu       sync.Mutex
	failures int
	until    time.Time
	now      func() time.Time
}

const (
	// freeAttempts is generous, because the common case is an operator who has
	// two passwords in their head and no way to see which one this is.
	freeAttempts = 5
	// The delay after that doubles per failure, from a quarter of a minute to
	// five. Bounded, because the same person who mistypes a password is the
	// one who has to install an update, and locking them out for an hour on an
	// appliance with no login screen to explain it is its own failure.
	firstLockout = 15 * time.Second
	maxLockout   = 5 * time.Minute
)

func (g *attemptGuard) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// wait reports how long the caller must wait before the next attempt is
// allowed. Zero means now.
func (g *attemptGuard) wait() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	remaining := g.until.Sub(g.clock())
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// record notes the outcome of an attempt. A correct password clears the
// history: somebody who knows it is not the person being kept out.
func (g *attemptGuard) record(correct bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if correct {
		g.failures = 0
		g.until = time.Time{}
		return
	}
	g.failures++
	if g.failures <= freeAttempts {
		return
	}
	lockout := firstLockout << (g.failures - freeAttempts - 1)
	if lockout > maxLockout || lockout <= 0 {
		lockout = maxLockout
	}
	g.until = g.clock().Add(lockout)
}

// refuseTooManyAttempts answers 429 and returns true when the caller is still
// serving out a lockout.
func (s *Server) refuseTooManyAttempts(w http.ResponseWriter) bool {
	wait := s.passwords.wait()
	if wait <= 0 {
		return false
	}
	seconds := int(wait.Round(time.Second).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests,
		"too many wrong passwords; try again in "+strconv.Itoa(seconds)+" seconds")
	return true
}
