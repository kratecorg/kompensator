package reconcile

import (
	"slices"
	"testing"
)

// TestSelectUsedEnvKeepsOnlyReferencedVariables verifies the rule that keeps a
// stack-wide variable from churning unrelated projects: a variable the compose
// model never interpolates cannot reach a container, so it must stay out of the
// config fingerprint. PG_PRIMARY_NODE is the motivating case — it only feeds a
// managed file, yet lives in the deploy env of every project of its stack.
func TestSelectUsedEnvKeepsOnlyReferencedVariables(t *testing.T) {
	extraEnv := []string{
		"CARIMCO_DB_PASSWORD=secret",
		"PG_PRIMARY_NODE=customer05",
		"POSTGRES_TAG=17.2",
	}
	used := map[string]bool{
		"CARIMCO_DB_PASSWORD": true,
		"POSTGRES_TAG":        true,
	}

	got := selectUsedEnv(extraEnv, used)

	want := []string{"CARIMCO_DB_PASSWORD=secret", "POSTGRES_TAG=17.2"}
	if !slices.Equal(got, want) {
		t.Errorf("selectUsedEnv() = %v, want %v", got, want)
	}
}

// TestSelectUsedEnvKeepsEmptyValues verifies that a referenced variable set to
// the empty string stays in the fingerprint: switching it between empty and set
// is a real change of the deploy.
func TestSelectUsedEnvKeepsEmptyValues(t *testing.T) {
	got := selectUsedEnv([]string{"REPLICATION_SLOT="}, map[string]bool{"REPLICATION_SLOT": true})

	if !slices.Equal(got, []string{"REPLICATION_SLOT="}) {
		t.Errorf("selectUsedEnv() = %v, want [REPLICATION_SLOT=]", got)
	}
}
