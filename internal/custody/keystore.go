package custody

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Keystore persistence constants.
const (
	keystoreVersion = 1
	kdfName         = "pbkdf2-sha256"
	pbkdf2Iters     = 600_000    // OWASP-style cost for PBKDF2-HMAC-SHA256
	minIters        = 100_000    // reject downgraded KDF cost on open
	maxIters        = 10_000_000 // DoS guard on a tampered/huge iters
	saltLen         = 16
	aesKeyLen       = 32 // AES-256
	// checkPlaintext is encrypted under the derived key so opening with the
	// wrong passphrase is detected immediately, even for an empty keystore.
	checkPlaintext = "custody-keystore-v1"
)

// ErrBadPassphrase is returned by Open when the passphrase does not match the
// keystore (the check value fails to decrypt/verify).
var ErrBadPassphrase = errors.New("custody: wrong passphrase for keystore")

// Entry is one stored wallet. The address is plaintext (needed for faucet and
// balance lookups without unlocking); only Secret is encrypted.
type Entry struct {
	Name    string `json:"-"`
	Chain   string `json:"chain"`
	Address string `json:"address"`
	Nonce   string `json:"nonce"`  // base64 AES-GCM nonce for Secret
	Secret  string `json:"secret"` // base64 AES-GCM ciphertext of the raw secret
}

type keystoreFile struct {
	Version  int              `json:"version"`
	KDF      string           `json:"kdf"`
	Salt     string           `json:"salt"` // base64
	Iters    int              `json:"iters"`
	Check    string           `json:"check"` // base64 nonce||ciphertext of checkPlaintext
	Accounts map[string]Entry `json:"accounts"`
}

// Keystore is an AES-256-GCM-encrypted wallet store. The encryption key is
// derived from a passphrase with PBKDF2-HMAC-SHA256 + a per-file random salt.
// Secrets are encrypted at rest; addresses are stored in the clear.
type Keystore struct {
	path string
	key  []byte
	file keystoreFile
}

// Open loads the keystore at path, deriving the key from passphrase and
// verifying it against the stored check value. If the file does not exist, a
// new keystore is created (parent dirs made, file mode 0600) with a fresh salt.
func Open(path, passphrase string) (*Keystore, error) {
	if passphrase == "" {
		return nil, errors.New("custody: empty passphrase (set CUSTODY_PASSPHRASE)")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return create(path, passphrase)
	}
	if err != nil {
		return nil, fmt.Errorf("custody: read keystore: %w", err)
	}

	var f keystoreFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("custody: parse keystore: %w", err)
	}
	if f.Version != keystoreVersion || f.KDF != kdfName {
		return nil, fmt.Errorf("custody: unsupported keystore (version %d, kdf %q)", f.Version, f.KDF)
	}
	salt, err := base64.StdEncoding.DecodeString(f.Salt)
	if err != nil {
		return nil, fmt.Errorf("custody: bad salt: %w", err)
	}
	// The KDF params come from the file (attacker-controllable if the file is
	// tampered). Reject implausible values: too few iters weakens an offline
	// passphrase attack; too many is a DoS; a wrong salt length is corruption.
	if len(salt) != saltLen {
		return nil, fmt.Errorf("custody: bad salt length %d (want %d)", len(salt), saltLen)
	}
	if f.Iters < minIters || f.Iters > maxIters {
		return nil, fmt.Errorf("custody: refusing keystore with implausible kdf iters %d", f.Iters)
	}
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, f.Iters, aesKeyLen)
	if err != nil {
		return nil, fmt.Errorf("custody: derive key: %w", err)
	}
	ks := &Keystore{path: path, key: key, file: f}
	if ks.file.Accounts == nil {
		ks.file.Accounts = make(map[string]Entry)
	}
	// Verify the passphrase via the check value.
	got, err := ks.decryptCombined(f.Check)
	if err != nil || subtle.ConstantTimeCompare(got, []byte(checkPlaintext)) != 1 {
		return nil, ErrBadPassphrase
	}
	return ks, nil
}

func create(path, passphrase string) (*Keystore, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("custody: salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, pbkdf2Iters, aesKeyLen)
	if err != nil {
		return nil, fmt.Errorf("custody: derive key: %w", err)
	}
	ks := &Keystore{
		path: path,
		key:  key,
		file: keystoreFile{
			Version:  keystoreVersion,
			KDF:      kdfName,
			Salt:     base64.StdEncoding.EncodeToString(salt),
			Iters:    pbkdf2Iters,
			Accounts: make(map[string]Entry),
		},
	}
	check, err := ks.encryptCombined([]byte(checkPlaintext))
	if err != nil {
		return nil, err
	}
	ks.file.Check = check
	if err := ks.persist(); err != nil {
		return nil, err
	}
	return ks, nil
}

// Put stores (or replaces) a wallet by name. secret is encrypted; address is
// stored plaintext. The keystore file is rewritten atomically.
func (ks *Keystore) Put(name, chain, address string, secret []byte) error {
	if name == "" {
		return errors.New("custody: empty wallet name")
	}
	nonce, ct, err := ks.encrypt(secret)
	if err != nil {
		return err
	}
	ks.file.Accounts[name] = Entry{
		Chain:   chain,
		Address: address,
		Nonce:   base64.StdEncoding.EncodeToString(nonce),
		Secret:  base64.StdEncoding.EncodeToString(ct),
	}
	return ks.persist()
}

// Get returns the chain id, address, and decrypted secret for name.
func (ks *Keystore) Get(name string) (chain, address string, secret []byte, err error) {
	e, ok := ks.file.Accounts[name]
	if !ok {
		return "", "", nil, fmt.Errorf("custody: no wallet %q", name)
	}
	nonce, err := base64.StdEncoding.DecodeString(e.Nonce)
	if err != nil {
		return "", "", nil, fmt.Errorf("custody: bad nonce: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(e.Secret)
	if err != nil {
		return "", "", nil, fmt.Errorf("custody: bad secret: %w", err)
	}
	secret, err = ks.decrypt(nonce, ct)
	if err != nil {
		return "", "", nil, fmt.Errorf("custody: decrypt secret: %w", err)
	}
	return e.Chain, e.Address, secret, nil
}

// List returns all wallets (name populated), sorted by name, without secrets.
func (ks *Keystore) List() []Entry {
	out := make([]Entry, 0, len(ks.file.Accounts))
	for name, e := range ks.file.Accounts {
		out = append(out, Entry{Name: name, Chain: e.Chain, Address: e.Address})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns the chain id and address for name (no decryption).
func (ks *Keystore) Lookup(name string) (chain, address string, ok bool) {
	e, ok := ks.file.Accounts[name]
	return e.Chain, e.Address, ok
}

// Delete removes a wallet by name (no-op if absent).
func (ks *Keystore) Delete(name string) error {
	if _, ok := ks.file.Accounts[name]; !ok {
		return nil
	}
	delete(ks.file.Accounts, name)
	return ks.persist()
}

// --- crypto helpers ---

func (ks *Keystore) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(ks.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encrypt returns a fresh random nonce and the AES-GCM ciphertext of plaintext.
func (ks *Keystore) encrypt(plaintext []byte) (nonce, ciphertext []byte, err error) {
	aead, err := ks.gcm()
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return nonce, aead.Seal(nil, nonce, plaintext, nil), nil
}

func (ks *Keystore) decrypt(nonce, ciphertext []byte) ([]byte, error) {
	aead, err := ks.gcm()
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errors.New("custody: bad nonce length")
	}
	return aead.Open(nil, nonce, ciphertext, nil)
}

// encryptCombined returns base64(nonce || ciphertext) — used for the check value.
func (ks *Keystore) encryptCombined(plaintext []byte) (string, error) {
	nonce, ct, err := ks.encrypt(plaintext)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

func (ks *Keystore) decryptCombined(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	aead, err := ks.gcm()
	if err != nil {
		return nil, err
	}
	if len(raw) < aead.NonceSize() {
		return nil, errors.New("custody: check value too short")
	}
	return ks.decrypt(raw[:aead.NonceSize()], raw[aead.NonceSize():])
}

// persist writes the keystore file with mode 0600 via a temp file + rename so a
// crash cannot leave a half-written keystore.
func (ks *Keystore) persist() error {
	if dir := filepath.Dir(ks.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("custody: mkdir keystore dir: %w", err)
		}
	}
	data, err := json.MarshalIndent(ks.file, "", "  ")
	if err != nil {
		return err
	}
	// This file is the sole copy of the wallet secrets, so fsync the temp file
	// before the atomic rename — a crash must not lose a just-created seed.
	tmp := ks.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("custody: write keystore: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("custody: write keystore: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("custody: sync keystore: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("custody: close keystore: %w", err)
	}
	if err := os.Rename(tmp, ks.path); err != nil {
		return fmt.Errorf("custody: replace keystore: %w", err)
	}
	return nil
}
