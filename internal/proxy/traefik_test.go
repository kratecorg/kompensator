package proxy

import (
	"strings"
	"testing"
)

func TestTraefikComposeTrustsPrivateProxyNetworks(t *testing.T) {
	compose, err := (traefik{}).Compose(ManagedSpec{
		Name:       "internal",
		Env:        "preprod",
		Stack:      "carimco",
		DynamicDir: "/tmp/dynamic",
		Networks:   []ManagedNetwork{{Name: "external"}},
	})
	if err != nil {
		t.Fatalf("Compose() error = %v", err)
	}

	config := string(compose)
	for _, option := range []string{
		"--entryPoints.web.address=:80",
		"--entryPoints.web.forwardedHeaders.trustedIPs=172.16.0.0/12,fd00::/8",
	} {
		if !strings.Contains(config, option) {
			t.Errorf("Compose() does not contain %q:\n%s", option, config)
		}
	}
}
