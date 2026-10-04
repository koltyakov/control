package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/store"
)

type Identity struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
	ID      string
	cert    tls.Certificate
}

func ID(public []byte) string {
	h := sha256.Sum256(public)
	return hex.EncodeToString(h[:])
}

func Load(dir string) (*Identity, error) {
	path := filepath.Join(dir, "identity.key")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		b = key
		err = store.Bytes(path, b, 0600)
	}
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid identity key")
	}
	priv := ed25519.PrivateKey(b)
	pub := priv.Public().(ed25519.PublicKey)
	i := &Identity{Private: priv, Public: pub, ID: ID(pub)}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: i.ID},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(1, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return nil, err
	}
	i.cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	return i, nil
}

// TLS pins the peer's enrolled public key instead of relying on DNS certificates.
func (i *Identity) TLS(peerID string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{i.cert},
		ClientAuth:         tls.RequireAnyClientCert,
		InsecureSkipVerify: true, // Verified against the enrolled key below.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) != 1 {
				return errors.New("expected one peer certificate")
			}
			cert := cs.PeerCertificates[0]
			pub, ok := cert.PublicKey.(ed25519.PublicKey)
			if !ok || ID(pub) != peerID {
				return errors.New("peer identity mismatch")
			}
			if time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
				return errors.New("expired peer certificate")
			}
			return nil
		},
	}
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
