package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// pauseFileName is the marker inside a kompensator home that suspends
// reconciling on this node.
const pauseFileName = "paused"

// idlePollInterval is how often WaitForIdle retries the run lock.
const idlePollInterval = time.Second

// Pause records that reconciling is deliberately suspended. Without it the only
// way to keep a cron tick out of a delicate operation — a database switchover,
// say — is to hold the run lock from outside, which leaves kompensator unaware
// that it is being held back and turns an implementation detail into an
// interface.
type Pause struct {
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since"`
	// Until is zero when the pause was set without an expiry. Every bounded
	// pause lifts by itself, because a forgotten pause silently switches off
	// GitOps and nothing else in the system would notice.
	Until time.Time `json:"until,omitempty"`
}

// IsExpired reports whether a bounded pause has run out.
func (p Pause) IsExpired(now time.Time) bool {
	return !p.Until.IsZero() && now.After(p.Until)
}

// Describe renders the pause for an operator, including how much of it is left.
func (p Pause) Describe(now time.Time) string {
	reason := p.Reason
	if reason == "" {
		reason = "no reason given"
	}
	if p.Until.IsZero() {
		return fmt.Sprintf("%s (since %s, until resumed)", reason, p.Since.Format(time.RFC3339))
	}
	return fmt.Sprintf("%s (since %s, expires %s, %s left)",
		reason, p.Since.Format(time.RFC3339), p.Until.Format(time.RFC3339),
		p.Until.Sub(now).Round(time.Second))
}

func pausePath(home string) string {
	return filepath.Join(home, pauseFileName)
}

// SetPause suspends reconciling on this node. A zero duration means "until
// someone resumes"; the caller is expected to make that choice explicitly.
func SetPause(home, reason string, duration time.Duration) (Pause, error) {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return Pause{}, fmt.Errorf("create home dir: %w", err)
	}
	p := Pause{Reason: reason, Since: time.Now()}
	if duration > 0 {
		p.Until = p.Since.Add(duration)
	}
	blob, err := marshalPause(p)
	if err != nil {
		return Pause{}, err
	}
	if err := os.WriteFile(pausePath(home), blob, 0o644); err != nil {
		return Pause{}, fmt.Errorf("write pause file: %w", err)
	}
	return p, nil
}

func marshalPause(p Pause) ([]byte, error) {
	blob, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode pause: %w", err)
	}
	return append(blob, '\n'), nil
}

// ClearPause resumes reconciling and reports whether a pause was actually
// lifted. Clearing a home that is not paused is not an error, so an operator
// can repeat the call after a half-finished operation.
func ClearPause(home string) (bool, error) {
	err := os.Remove(pausePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remove pause file: %w", err)
	}
	return true, nil
}

// ReadPause returns the pause marker of a home, if one exists. It has no side
// effects: expiry is decided by the caller, so a status view can show an
// expired pause instead of quietly deleting it.
//
// A marker that cannot be parsed is reported as an error rather than as "not
// paused" — resuming behind the operator's back is the one outcome that must
// not happen.
func ReadPause(home string) (Pause, bool, error) {
	blob, err := os.ReadFile(pausePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return Pause{}, false, nil
	}
	if err != nil {
		return Pause{}, false, fmt.Errorf("read pause file: %w", err)
	}
	var p Pause
	if err := json.Unmarshal(blob, &p); err != nil {
		return Pause{}, false, fmt.Errorf("parse pause file %s: %w", pausePath(home), err)
	}
	return p, true, nil
}

// WaitForIdle blocks until no reconcile holds the run lock, or until the
// timeout passes. A zero timeout makes it a single attempt.
//
// This is what turns the pause marker into a usable precondition: the marker
// keeps later runs out, this waits for the one that may already be underway.
// Because a run checks the marker only after taking the lock, an idle moment
// observed here cannot be followed by a run that ignores the pause.
func WaitForIdle(ctx context.Context, home string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		unlock, held, err := lock(home)
		if err != nil {
			return err
		}
		if held {
			unlock()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("a reconcile is still running after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(idlePollInterval):
		}
	}
}

// isPaused reports whether this run must stand down. An expired marker is
// removed here so a node recovers on its own even if nobody ever calls resume.
func isPaused(log *slog.Logger, home string) (bool, error) {
	p, found, err := ReadPause(home)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if p.IsExpired(time.Now()) {
		if _, err := ClearPause(home); err != nil {
			return false, err
		}
		log.Info("pause expired, resuming", "reason", p.Reason, "expired", p.Until)
		return false, nil
	}
	log.Info("reconcile paused, skipping", "reason", p.Reason, "until", p.Until)
	return true, nil
}
