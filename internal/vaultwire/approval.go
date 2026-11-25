package vaultwire

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for approval keys. Changing them changes every derived
// key, so they are part of the stored profile contract.
const (
	approvalArgonTime    = 3
	approvalArgonMemory  = 64 * 1024 // KiB
	approvalArgonThreads = 4
	approvalSaltDomain   = "control-vault-approval-key-v1\x00"
	// MinApprovalSaltSize is the smallest salt NewApprovalKey accepts.
	MinApprovalSaltSize = 16
	maxApprovalSaltSize = 1024
)

// NewApprovalSalt returns a random salt suitable for NewApprovalKey. Store it
// with the profile alongside the derived public key.
func NewApprovalSalt() ([]byte, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return salt, nil
}

// NewApprovalKey derives the approval signing key from a vault passphrase with
// Argon2id. The broker only needs the public half. A domain prefix keeps the
// derivation distinct from other uses of the same passphrase and salt.
func NewApprovalKey(pass []byte, salt []byte) (ed25519.PrivateKey, error) {
	if len(pass) == 0 {
		return nil, errors.New("approval passphrase is empty")
	}
	if len(salt) < MinApprovalSaltSize || len(salt) > maxApprovalSaltSize {
		return nil, errors.New("approval salt must be between 16 and 1024 bytes")
	}
	domainSalt := make([]byte, 0, len(approvalSaltDomain)+len(salt))
	domainSalt = append(domainSalt, approvalSaltDomain...)
	domainSalt = append(domainSalt, salt...)
	seed := argon2.IDKey(pass, domainSalt, approvalArgonTime, approvalArgonMemory, approvalArgonThreads, ed25519.SeedSize)
	key := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	return key, nil
}

// NewCertificate creates a self-signed broker certificate for key. Trust comes
// from Pin over the returned leaf DER, not from names or validity dates.
func NewCertificate(key ed25519.PrivateKey) (tls.Certificate, error) {
	if len(key) != ed25519.PrivateKeySize {
		return tls.Certificate{}, errors.New("invalid broker key")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: serverName},
		NotBefore:    time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
