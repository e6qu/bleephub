package store

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/crypto/nacl/box"
)

// ActionsVariable is an Actions configuration variable. Visibility and
// SelectedRepoIDs are populated only at the organization level.
type ActionsVariable struct {
	Name            string    `json:"name"`
	Value           string    `json:"value"`
	Visibility      string    `json:"visibility,omitempty"`
	SelectedRepoIDs []int     `json:"selected_repository_ids,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// OrgSecret is an organization-level Actions secret plus its visibility scoping.
type OrgSecret struct {
	Secret
	Visibility      string `json:"visibility"`
	SelectedRepoIDs []int  `json:"selected_repository_ids,omitempty"`
}

// SecretsKeyPair is the X25519 keypair backing the Actions sealed-box contract.
// Persisted so key_id stays stable across restarts for clients caching the public key.
type SecretsKeyPair struct {
	KeyID      string `json:"key_id"`
	PublicKey  string `json:"public_key"`  // base64 32-byte X25519 public key
	PrivateKey string `json:"private_key"` // base64 32-byte X25519 private key
}

// TimelineRecord is the slice of the runner's timeline record bleephub consumes
// for per-step status, timing and log association. Type is "Job" for the job
// record and "Task" for each step.
type TimelineRecord struct {
	ID         string          `json:"id"`
	ParentID   string          `json:"parentId"`
	Type       string          `json:"type"`
	Name       string          `json:"name"`
	RefName    string          `json:"refName"`
	Order      int             `json:"order"`
	State      string          `json:"state"`  // pending | inProgress | completed
	Result     string          `json:"result"` // succeeded | failed | skipped | canceled | abandoned
	StartTime  string          `json:"startTime"`
	FinishTime string          `json:"finishTime"`
	Log        *TimelineLogRef `json:"log"`
}

// TimelineLogRef points a timeline record at its uploaded log file.
type TimelineLogRef struct {
	ID int `json:"id"`
}

// ActionsKeyPair returns the server-wide sealed-box keypair, generating and
// persisting it on first use.
func (st *Store) ActionsKeyPair() (*SecretsKeyPair, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.actionsKeyPair != nil {
		return st.actionsKeyPair, nil
	}
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate actions secrets keypair: %w", err)
	}
	// Derive the decimal key_id from the public key so the two can never disagree.
	kp := &SecretsKeyPair{
		KeyID:      strconv.FormatUint(binary.BigEndian.Uint64(pub[:8]), 10),
		PublicKey:  base64.StdEncoding.EncodeToString(pub[:]),
		PrivateKey: base64.StdEncoding.EncodeToString(priv[:]),
	}
	st.actionsKeyPair = kp
	if st.Persist != nil {
		st.Persist.MustPut("actions_crypto", "keypair", kp)
	}
	return kp, nil
}

// OpenSealedSecret decrypts a base64 libsodium sealed-box ciphertext produced
// against the server's Actions public key.
func (st *Store) OpenSealedSecret(encryptedValue string) (string, error) {
	kp, err := st.ActionsKeyPair()
	if err != nil {
		return "", err
	}
	ct, err := base64.StdEncoding.DecodeString(encryptedValue)
	if err != nil {
		return "", fmt.Errorf("encrypted_value is not valid base64: %w", err)
	}
	pubRaw, err := base64.StdEncoding.DecodeString(kp.PublicKey)
	if err != nil {
		return "", fmt.Errorf("stored public key corrupt: %w", err)
	}
	privRaw, err := base64.StdEncoding.DecodeString(kp.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("stored private key corrupt: %w", err)
	}
	var pub, priv [32]byte
	copy(pub[:], pubRaw)
	copy(priv[:], privRaw)
	plain, ok := box.OpenAnonymous(nil, ct, &pub, &priv)
	if !ok {
		return "", fmt.Errorf("sealed box does not open with the server's key (wrong key_id or corrupt ciphertext)")
	}
	return string(plain), nil
}

// SealSecretValue encrypts a plaintext against the server's own Actions public
// key — the client side of the sealed-box contract.
func (st *Store) SealSecretValue(plaintext string) (encryptedValue, keyID string, err error) {
	kp, err := st.ActionsKeyPair()
	if err != nil {
		return "", "", err
	}
	pubRaw, err := base64.StdEncoding.DecodeString(kp.PublicKey)
	if err != nil {
		return "", "", fmt.Errorf("stored public key corrupt: %w", err)
	}
	var pub [32]byte
	copy(pub[:], pubRaw)
	ct, err := box.SealAnonymous(nil, []byte(plaintext), &pub, rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("seal secret value: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ct), kp.KeyID, nil
}
