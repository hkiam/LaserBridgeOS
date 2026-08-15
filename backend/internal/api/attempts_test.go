package api

import (
	"testing"
	"time"
)

func TestGuessingGetsSlowerAndKnowingDoesNot(t *testing.T) {
	now := time.Unix(1700000000, 0)
	guard := &attemptGuard{now: func() time.Time { return now }}

	// The operator with two passwords in their head is not the threat, so the
	// first few misses cost nothing.
	for i := 0; i < freeAttempts; i++ {
		guard.record(false)
		if wait := guard.wait(); wait != 0 {
			t.Fatalf("attempt %d was already delayed by %s", i+1, wait)
		}
	}

	guard.record(false)
	if wait := guard.wait(); wait != firstLockout {
		t.Fatalf("wait = %s, want %s after the first attempt past the allowance", wait, firstLockout)
	}
	// Waiting it out and getting it wrong again costs more, and the growth is
	// bounded so a mistyped password cannot lock an appliance out for an hour.
	now = now.Add(firstLockout)
	guard.record(false)
	if wait := guard.wait(); wait != 2*firstLockout {
		t.Fatalf("wait = %s, want the lockout to double", wait)
	}
	for i := 0; i < 20; i++ {
		now = now.Add(maxLockout)
		guard.record(false)
	}
	if wait := guard.wait(); wait != maxLockout {
		t.Fatalf("wait = %s, want it capped at %s", wait, maxLockout)
	}

	// Somebody who knows the password is not the person being kept out.
	guard.record(true)
	if wait := guard.wait(); wait != 0 {
		t.Fatalf("wait = %s after a correct password, want none", wait)
	}
}
