// Package identity manages a device's Ed25519 keypair — the machine's stable,
// revocable identity on a hub. The private key never leaves the machine.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// Identity is a device's Ed25519 keypair.
type Identity struct {
	Pub  ed25519.PublicKey
	Priv ed25519.PrivateKey
}

const (
	privName = "device.key"
	pubName  = "device.key.pub"
)

// Generate makes a fresh device identity.
func Generate() (Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Pub: pub, Priv: priv}, nil
}

// Fingerprint is a short, stable id derived from the public key.
func (id Identity) Fingerprint() string {
	sum := sha256.Sum256(id.Pub)
	return hex.EncodeToString(sum[:8]) // 16 hex chars
}

// Save writes the keypair under dir atomically (private key 0600, dir 0700).
func (id Identity) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	priv := pem.EncodeToMemory(&pem.Block{Type: "DEVBOX PRIVATE KEY", Bytes: id.Priv})
	if err := writeFileAtomic(filepath.Join(dir, privName), priv, 0o600); err != nil {
		return err
	}
	pub := pem.EncodeToMemory(&pem.Block{Type: "DEVBOX PUBLIC KEY", Bytes: id.Pub})
	return writeFileAtomic(filepath.Join(dir, pubName), pub, 0o644)
}

// writeFileAtomic writes data to path via a temp file + rename. CreateTemp makes
// the temp 0600, so the private key is never briefly world-readable.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a keypair previously written by Save.
func Load(dir string) (Identity, error) {
	raw, err := os.ReadFile(filepath.Join(dir, privName))
	if err != nil {
		return Identity{}, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "DEVBOX PRIVATE KEY" {
		return Identity{}, fmt.Errorf("identity: bad private key PEM in %s", dir)
	}
	priv := ed25519.PrivateKey(block.Bytes)
	if len(priv) != ed25519.PrivateKeySize {
		return Identity{}, fmt.Errorf("identity: private key wrong size %d", len(priv))
	}
	return Identity{Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}, nil
}

// LoadOrCreate loads the identity in dir, creating and saving one if absent.
// It is idempotent: an existing identity is never regenerated.
func LoadOrCreate(dir string) (Identity, error) {
	if _, err := os.Stat(filepath.Join(dir, privName)); err == nil {
		return Load(dir)
	} else if !os.IsNotExist(err) {
		return Identity{}, err
	}
	id, err := Generate()
	if err != nil {
		return Identity{}, err
	}
	return id, id.Save(dir)
}
