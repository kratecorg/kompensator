package reconcile

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPauseRoundTrip(t *testing.T) {
	home := t.TempDir()

	if _, found, err := ReadPause(home); err != nil || found {
		t.Fatalf("fresh home: found=%v err=%v", found, err)
	}

	set, err := SetPause(home, "db switchover", 15*time.Minute)
	if err != nil {
		t.Fatalf("SetPause: %v", err)
	}
	if set.Until.IsZero() {
		t.Error("a bounded pause must carry an expiry")
	}

	got, found, err := ReadPause(home)
	if err != nil || !found {
		t.Fatalf("ReadPause: found=%v err=%v", found, err)
	}
	if got.Reason != "db switchover" {
		t.Errorf("reason = %q, want %q", got.Reason, "db switchover")
	}
	if !got.Until.Equal(set.Until.Round(0)) && got.Until.Sub(set.Until).Abs() > time.Second {
		t.Errorf("until = %v, want %v", got.Until, set.Until)
	}
}

func TestPauseWithoutTimeoutHasNoExpiry(t *testing.T) {
	home := t.TempDir()

	p, err := SetPause(home, "", 0)
	if err != nil {
		t.Fatalf("SetPause: %v", err)
	}
	if !p.Until.IsZero() {
		t.Errorf("until = %v, want zero for an unbounded pause", p.Until)
	}
	if p.IsExpired(time.Now().Add(365 * 24 * time.Hour)) {
		t.Error("an unbounded pause must never expire")
	}
}

func TestPauseIsExpired(t *testing.T) {
	now := time.Now()
	p := Pause{Since: now, Until: now.Add(time.Minute)}

	if p.IsExpired(now.Add(30 * time.Second)) {
		t.Error("expired before its time")
	}
	if !p.IsExpired(now.Add(2 * time.Minute)) {
		t.Error("did not expire after its time")
	}
}

func TestClearPauseIsRepeatable(t *testing.T) {
	home := t.TempDir()
	if _, err := SetPause(home, "", time.Minute); err != nil {
		t.Fatalf("SetPause: %v", err)
	}

	lifted, err := ClearPause(home)
	if err != nil || !lifted {
		t.Fatalf("first clear: lifted=%v err=%v", lifted, err)
	}
	lifted, err = ClearPause(home)
	if err != nil {
		t.Fatalf("second clear: %v", err)
	}
	if lifted {
		t.Error("second clear reported a lift, want none")
	}
}

func TestReadPauseRejectsUnreadableMarker(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, pauseFileName), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// Treating a corrupt marker as "not paused" would resume reconciling behind
	// the operator's back, which is the one outcome that must not happen.
	if _, _, err := ReadPause(home); err == nil {
		t.Fatal("a corrupt marker must be an error, not an absent pause")
	}
}

func TestIsPausedDropsExpiredMarker(t *testing.T) {
	home := t.TempDir()
	expired := Pause{Reason: "stale", Since: time.Now().Add(-2 * time.Hour), Until: time.Now().Add(-time.Hour)}
	writePauseForTest(t, home, expired)

	paused, err := isPaused(discardLogger(), home)
	if err != nil {
		t.Fatalf("isPaused: %v", err)
	}
	if paused {
		t.Error("an expired pause must not hold a run back")
	}
	if _, found, _ := ReadPause(home); found {
		t.Error("an expired marker must be removed so the node recovers on its own")
	}
}

func TestIsPausedHoldsRunBack(t *testing.T) {
	home := t.TempDir()
	if _, err := SetPause(home, "db switchover", time.Hour); err != nil {
		t.Fatalf("SetPause: %v", err)
	}

	paused, err := isPaused(discardLogger(), home)
	if err != nil {
		t.Fatalf("isPaused: %v", err)
	}
	if !paused {
		t.Error("an active pause must hold a run back")
	}
}

func TestWaitForIdleReturnsWhenNoRunHoldsTheLock(t *testing.T) {
	if err := WaitForIdle(context.Background(), t.TempDir(), 0); err != nil {
		t.Fatalf("WaitForIdle on an idle home: %v", err)
	}
}

func TestWaitForIdleFailsWhileAReconcileHoldsTheLock(t *testing.T) {
	home := t.TempDir()
	unlock, held, err := lock(home)
	if err != nil || !held {
		t.Fatalf("take lock: held=%v err=%v", held, err)
	}
	defer unlock()

	if err := WaitForIdle(context.Background(), home, 0); err == nil {
		t.Fatal("WaitForIdle must not report an idle home while the lock is held")
	}
}

func writePauseForTest(t *testing.T, home string, p Pause) {
	t.Helper()
	blob, err := marshalPause(p)
	if err != nil {
		t.Fatalf("encode pause: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, pauseFileName), blob, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
