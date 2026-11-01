// Package pgbackrest runs the Node's pgBackRest repo host and issues the TLS
// identities PostgresDifferential databases use to reach it.
//
// Each Node has its own certificate authority. A repo host serves only the
// Projects placed on its own Node, so it never has to trust a certificate from
// anywhere else: the CA, and every private key it signs for, stays on the Node
// and is never sent over the network.
package pgbackrest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// RepoAlias is the name the repo host answers to on every Project network,
// and the name its server certificate is issued for.
const RepoAlias = "pgbackrest-repo"

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 365 * 24 * time.Hour
	renewBefore  = 30 * 24 * time.Hour
	certFileMode = 0o644
	keyFileMode  = 0o600
	// identityDirMode lets the container's user enter the directory; the
	// Agent keeps owning it so it can replace the files when it reissues.
	identityDirMode = 0o755
)

// CA is the Node's certificate authority.
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey
}

// LoadOrCreateCA returns the CA kept in dir, creating it on first use. The key
// is readable only by the Agent.
func LoadOrCreateCA(dir, nodeName string, now time.Time) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if certPEM, err := os.ReadFile(certPath); err == nil {
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("pgbackrest: read CA key: %w", err)
		}
		return parseCA(certPEM, keyPEM)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("pgbackrest: read CA: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pgbackrest: generate CA key: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: "cara pgbackrest CA " + nodeName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("pgbackrest: create CA: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pgbackrest: create CA dir: %w", err)
	}
	// Key first: a certificate without its key would be loaded on the next
	// start and fail there, while a key without a certificate is overwritten.
	if err := writeFile(keyPath, keyPEM, keyFileMode); err != nil {
		return nil, err
	}
	if err := writeFile(certPath, certPEM, certFileMode); err != nil {
		return nil, err
	}
	return parseCA(certPEM, keyPEM)
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, fmt.Errorf("pgbackrest: parse CA: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("pgbackrest: parse CA key: no PEM block")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pgbackrest: parse CA key: %w", err)
	}
	return &CA{cert: cert, certPEM: certPEM, key: key}, nil
}

// CertPEM is the CA certificate, for clients and the server to trust.
func (ca *CA) CertPEM() []byte { return ca.certPEM }

// IssueServer makes sure dir holds a valid server identity for the repo host,
// and returns the files that must belong to the user pgBackRest runs as.
func (ca *CA) IssueServer(dir, nodeName string, now time.Time) ([]string, error) {
	return ca.issue(dir, "server", nodeName, []string{RepoAlias}, x509.ExtKeyUsageServerAuth, now)
}

// IssueClient makes sure dir holds a valid client identity for one Project
// lifetime, and returns the files that must belong to the user pgBackRest runs
// as. The CN is the Project UID, which the repo host maps to the lifetime's
// stanzas, so a same-name successor never shares an identity.
func (ca *CA) IssueClient(dir, projectUID string, now time.Time) ([]string, error) {
	if projectUID == "" {
		return nil, errors.New("pgbackrest: client certificate needs a Project UID")
	}
	return ca.issue(dir, "client", projectUID, nil, x509.ExtKeyUsageClientAuth, now)
}

// issue writes <name>.crt, <name>.key and ca.crt into dir unless a valid
// identity for cn, signed by this CA and not close to expiry, is already
// there. The files are written as the Agent; handing them to the container's
// user is the caller's step, because how depends on whether the Agent is root.
func (ca *CA) issue(dir, name, cn string, dnsNames []string, usage x509.ExtKeyUsage, now time.Time) ([]string, error) {
	certPath := filepath.Join(dir, name+".crt")
	keyPath := filepath.Join(dir, name+".key")
	caPath := filepath.Join(dir, "ca.crt")
	files := []string{certPath, keyPath, caPath}
	if ca.stillValid(certPath, keyPath, cn, now) {
		return files, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pgbackrest: generate %s key: %w", name, err)
	}
	template := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("pgbackrest: sign %s certificate: %w", name, err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, identityDirMode); err != nil {
		return nil, fmt.Errorf("pgbackrest: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, identityDirMode); err != nil {
		return nil, fmt.Errorf("pgbackrest: chmod %s: %w", dir, err)
	}
	if err := writeFile(caPath, ca.certPEM, certFileMode); err != nil {
		return nil, err
	}
	if err := writeFile(keyPath, keyPEM, keyFileMode); err != nil {
		return nil, err
	}
	if err := writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), certFileMode); err != nil {
		return nil, err
	}
	return files, nil
}

// stillValid reports whether certPath holds a certificate for cn, signed by
// this CA, outside its renewal window, with its key beside it.
func (ca *CA) stillValid(certPath, keyPath, cn string, now time.Time) bool {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	if _, err := os.Stat(keyPath); err != nil {
		return false
	}
	cert, err := parseCert(certPEM)
	if err != nil || cert.Subject.CommonName != cn || now.Add(renewBefore).After(cert.NotAfter) {
		return false
	}
	return cert.CheckSignatureFrom(ca.cert) == nil
}

func parseCert(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("pgbackrest: encode key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func newSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		// crypto/rand failing means the process cannot make any key safely.
		panic(fmt.Sprintf("pgbackrest: random serial: %v", err))
	}
	return n
}

// writeFile replaces path with data through a temporary file in the same
// directory, so a reader never sees a half-written file. Identities are
// mounted as directories, never as single files, so the rename is visible
// inside running containers.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("pgbackrest: write %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pgbackrest: write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pgbackrest: chmod %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pgbackrest: sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pgbackrest: write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("pgbackrest: install %s: %w", path, err)
	}
	return nil
}
