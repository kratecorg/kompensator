package repo

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// envWithSecrets is the reference fixture for file-secret declarations: a pinned
// and an unpinned secret, an explicit mode, and both reload strategies.
const envWithSecrets = `
name: infra
stacks:
  - name: edge
    nodes:
      - customer05
      - customer06
secrets:
  - name: carimco-cert
    nodes:
      - customer05
      - customer06
    file:
      path: /srv/certs/carimco.pem
      mode: "0640"
    reload:
      command: [docker, exec, "{{container:edge/haproxy}}", kill, -HUP, "1"]
  - name: env-wide
    file:
      path: /srv/other.pem
    reload:
      recreate: edge/haproxy
`

func loadEnvWithSecrets(t *testing.T) Environment {
	t.Helper()
	var e Environment
	if err := yaml.Unmarshal([]byte(envWithSecrets), &e); err != nil {
		t.Fatalf("unmarshal env: %v", err)
	}
	return e
}

func TestEnvSecretParsing(t *testing.T) {
	e := loadEnvWithSecrets(t)
	if len(e.Secrets) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(e.Secrets))
	}
	cert, ok := e.Secret("carimco-cert")
	if !ok {
		t.Fatal("carimco-cert not found")
	}
	if cert.File == nil || cert.File.Path != "/srv/certs/carimco.pem" {
		t.Errorf("unexpected file: %+v", cert.File)
	}
	if cert.Reload == nil || len(cert.Reload.Command) != 6 {
		t.Errorf("unexpected reload command: %+v", cert.Reload)
	}
}

func TestEnvSecretFileMode(t *testing.T) {
	e := loadEnvWithSecrets(t)
	cert, _ := e.Secret("carimco-cert")
	mode, err := cert.FileMode()
	if err != nil {
		t.Fatalf("FileMode: %v", err)
	}
	if mode != os.FileMode(0o640) {
		t.Errorf("mode = %o, want 0640", mode)
	}

	wide, _ := e.Secret("env-wide")
	mode, err = wide.FileMode()
	if err != nil {
		t.Fatalf("FileMode default: %v", err)
	}
	if mode != os.FileMode(defaultSecretFileMode) {
		t.Errorf("default mode = %o, want %o", mode, defaultSecretFileMode)
	}
}

func TestEnvSecretTargetsNode(t *testing.T) {
	e := loadEnvWithSecrets(t)
	cert, _ := e.Secret("carimco-cert")
	if !cert.TargetsNode("customer05") {
		t.Error("pinned secret should target listed node")
	}
	if cert.TargetsNode("customer02") {
		t.Error("pinned secret should not target unlisted node")
	}

	wide, _ := e.Secret("env-wide")
	if !wide.TargetsNode("any-node") {
		t.Error("unpinned secret should target every node")
	}
}

func TestEnvSecretValidate(t *testing.T) {
	cases := map[string]struct {
		secret  EnvSecret
		wantErr bool
	}{
		"ok": {
			secret:  EnvSecret{Name: "a", File: &SecretFile{Path: "/x"}},
			wantErr: false,
		},
		"no name": {
			secret:  EnvSecret{File: &SecretFile{Path: "/x"}},
			wantErr: true,
		},
		"no file": {
			secret:  EnvSecret{Name: "a"},
			wantErr: true,
		},
		"empty path": {
			secret:  EnvSecret{Name: "a", File: &SecretFile{}},
			wantErr: true,
		},
		"bad mode": {
			secret:  EnvSecret{Name: "a", File: &SecretFile{Path: "/x", Mode: "xyz"}},
			wantErr: true,
		},
		"both reload strategies": {
			secret: EnvSecret{
				Name: "a",
				File: &SecretFile{Path: "/x"},
				Reload: &SecretReload{
					Command:  []string{"true"},
					Recreate: "s/p",
				},
			},
			wantErr: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.secret.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestParticipatingNodes(t *testing.T) {
	e := loadEnvWithSecrets(t)
	all := []string{"customer02", "customer05", "customer06"}
	got := e.ParticipatingNodes(all)
	want := []string{"customer05", "customer06"}
	if len(got) != len(want) {
		t.Fatalf("participating nodes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("participating nodes = %v, want %v", got, want)
		}
	}
}

func TestSecretFileBlobPath(t *testing.T) {
	got := SecretFileBlob("/repo", "infra", "carimco-cert")
	want := "/repo/environments/infra/secrets/files/carimco-cert.age"
	if got != want {
		t.Errorf("SecretFileBlob = %q, want %q", got, want)
	}
}
