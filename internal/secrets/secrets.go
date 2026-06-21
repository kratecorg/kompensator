// Package secrets implements GitOps secret distribution (variant A): secret
// values live age-encrypted in the deployment repo and are decrypted locally on
// each node with its own age identity at reconcile time, so the plaintext never
// touches Git. The controller holds an admin identity used to author (edit /
// rekey) secrets; every node receives a unique identity at bootstrap and its
// public recipient is stored in the inventory so the controller can encrypt for
// it. Decryption is native (filippo.io/age), so nodes need no extra binaries.
package secrets

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
	"gopkg.in/yaml.v3"
)

// keyFile is the age identity file name within a kompensator home.
const keyFile = "age.key"

// KeyPath returns the age identity path inside a kompensator home.
func KeyPath(home string) string {
	return filepath.Join(home, keyFile)
}

// GenerateIdentity creates a new X25519 age identity, returning the private key
// ("AGE-SECRET-KEY-1...") and the public recipient ("age1...").
func GenerateIdentity() (privateKey, recipient string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", fmt.Errorf("generate age identity: %w", err)
	}
	return id.String(), id.Recipient().String(), nil
}

// RecipientFromIdentity derives the public recipient from a private key string.
func RecipientFromIdentity(privateKey string) (string, error) {
	id, err := age.ParseX25519Identity(strings.TrimSpace(privateKey))
	if err != nil {
		return "", fmt.Errorf("parse age identity: %w", err)
	}
	return id.Recipient().String(), nil
}

// WriteIdentity writes a private key to path with 0600 permissions.
func WriteIdentity(path, privateKey string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create dir for identity: %w", err)
	}
	if err := os.WriteFile(path, []byte(privateKey+"\n"), 0o600); err != nil {
		return fmt.Errorf("write identity %s: %w", path, err)
	}
	return nil
}

// LoadOrCreateIdentity returns the recipient for the identity at <home>/age.key,
// generating and storing a new one (0600) when the file does not yet exist.
// created reports whether a fresh key was written.
func LoadOrCreateIdentity(home string) (recipient string, created bool, err error) {
	path := KeyPath(home)
	data, err := os.ReadFile(path)
	if err == nil {
		rec, err := RecipientFromIdentity(string(data))
		return rec, false, err
	}
	if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("read identity %s: %w", path, err)
	}
	priv, rec, err := GenerateIdentity()
	if err != nil {
		return "", false, err
	}
	if err := WriteIdentity(path, priv); err != nil {
		return "", false, err
	}
	return rec, true, nil
}

// Encrypt armor-encrypts plaintext for the given recipients ("age1...").
func Encrypt(recipients []string, plaintext []byte) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients to encrypt for")
	}
	recs := make([]age.Recipient, 0, len(recipients))
	for _, r := range recipients {
		rec, err := age.ParseX25519Recipient(strings.TrimSpace(r))
		if err != nil {
			return nil, fmt.Errorf("parse recipient %q: %w", r, err)
		}
		recs = append(recs, rec)
	}
	var buf bytes.Buffer
	armorWriter := armor.NewWriter(&buf)
	w, err := age.Encrypt(armorWriter, recs...)
	if err != nil {
		return nil, fmt.Errorf("init age encrypt: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("age write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("age close: %w", err)
	}
	if err := armorWriter.Close(); err != nil {
		return nil, fmt.Errorf("armor close: %w", err)
	}
	return buf.Bytes(), nil
}

// Decrypt decrypts armored age ciphertext using the identity at identityPath.
func Decrypt(identityPath string, ciphertext []byte) ([]byte, error) {
	keyData, err := os.ReadFile(identityPath)
	if err != nil {
		return nil, fmt.Errorf("read identity %s: %w", identityPath, err)
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(string(keyData)))
	if err != nil {
		return nil, fmt.Errorf("parse identity %s: %w", identityPath, err)
	}
	armorReader := armor.NewReader(bytes.NewReader(ciphertext))
	r, err := age.Decrypt(armorReader, id)
	if err != nil {
		return nil, fmt.Errorf("age decrypt: %w", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read decrypted: %w", err)
	}
	return out, nil
}

// EncryptMap marshals a flat string map to YAML and encrypts it for recipients.
func EncryptMap(recipients []string, values map[string]string) ([]byte, error) {
	plaintext, err := yaml.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal secrets: %w", err)
	}
	return Encrypt(recipients, plaintext)
}

// DecryptMap decrypts ciphertext with the identity at identityPath and parses
// the YAML plaintext into a flat string map.
func DecryptMap(identityPath string, ciphertext []byte) (map[string]string, error) {
	plaintext, err := Decrypt(identityPath, ciphertext)
	if err != nil {
		return nil, err
	}
	var values map[string]string
	if err := yaml.Unmarshal(plaintext, &values); err != nil {
		return nil, fmt.Errorf("parse decrypted secrets: %w", err)
	}
	return values, nil
}
