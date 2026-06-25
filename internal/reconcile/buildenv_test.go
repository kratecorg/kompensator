package reconcile

import (
	"slices"
	"testing"

	"kompensator/internal/repo"
	"kompensator/internal/runtime"
)

// TestBuildEnvExcludesOneShotFromRefs verifies that a one-shot job service is
// kept out of the running-image refs (so an exited job is not seen as drift)
// while its image/tag are still injected into the deploy environment (and thus
// the config hash, which drives a redeploy when the job's tag changes).
func TestBuildEnvExcludesOneShotFromRefs(t *testing.T) {
	names := runtime.Names{Repo: "cd", Node: "node1"}
	desired := map[string]repo.ServiceImage{
		"frontend": {Image: "reg/frontend", Tag: "v1"},
		"apk-distribution": {
			Image:   "reg/carimco-apk",
			Tag:     "preprod-v1",
			OneShot: true,
		},
	}

	extraEnv, refs := buildEnv(names, "preprod", "carimco", "app", "/proxy", nil, desired)

	if _, ok := refs["apk-distribution"]; ok {
		t.Errorf("one-shot service must not appear in image refs, got %v", refs)
	}
	if refs["frontend"] != "reg/frontend:v1" {
		t.Errorf("long-running service missing from refs: %v", refs)
	}

	wantInjected := []string{
		"APK_DISTRIBUTION_IMAGE=reg/carimco-apk",
		"APK_DISTRIBUTION_TAG=preprod-v1",
	}
	for _, want := range wantInjected {
		if !slices.Contains(extraEnv, want) {
			t.Errorf("expected %q to be injected into extraEnv, got %v", want, extraEnv)
		}
	}
}
