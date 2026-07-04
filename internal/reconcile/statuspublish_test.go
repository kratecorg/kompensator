package reconcile

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStatusChangedSincePublishIgnoresTimestamp verifies that a status document
// differing only in its reconciledAt timestamp is not treated as a change (so a
// node reconciling every minute does not force-push an identical status), while
// a substantive change still is.
func TestStatusChangedSincePublishIgnoresTimestamp(t *testing.T) {
	gitDir := t.TempDir()
	statusPath := filepath.Join(gitDir, "status")
	if err := os.MkdirAll(statusPath, 0o755); err != nil {
		t.Fatal(err)
	}
	published := "node: c02\nenv: preprod\ndesiredCommit: abc\nreconciledAt: \"2026-07-04T10:00:00Z\"\nhealthy: true\n"
	if err := os.WriteFile(filepath.Join(statusPath, "preprod.yml"), []byte(published), 0o644); err != nil {
		t.Fatal(err)
	}

	sameButNewer := map[string][]byte{
		"status/preprod.yml": []byte("node: c02\nenv: preprod\ndesiredCommit: abc\nreconciledAt: \"2026-07-04T10:01:00Z\"\nhealthy: true\n"),
	}
	changed, err := statusChangedSincePublish(gitDir, sameButNewer)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("timestamp-only difference must not count as a change")
	}

	substantive := map[string][]byte{
		"status/preprod.yml": []byte("node: c02\nenv: preprod\ndesiredCommit: def\nreconciledAt: \"2026-07-04T10:01:00Z\"\nhealthy: true\n"),
	}
	changed, err = statusChangedSincePublish(gitDir, substantive)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a changed desiredCommit must count as a change")
	}
}

// TestStatusChangedSincePublishFirstRun verifies that an absent gitDir (nothing
// published yet) counts as a change so the first reconcile always publishes.
func TestStatusChangedSincePublishFirstRun(t *testing.T) {
	gitDir := filepath.Join(t.TempDir(), "never-published")
	files := map[string][]byte{
		"status/preprod.yml": []byte("node: c02\nreconciledAt: \"2026-07-04T10:00:00Z\"\n"),
	}
	changed, err := statusChangedSincePublish(gitDir, files)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first publish must count as a change")
	}
}
