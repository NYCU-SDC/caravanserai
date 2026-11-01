package pgbackrest

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	cert, err := parseCert(data)
	require.NoError(t, err)
	return cert
}

func TestCAIsCreatedOnceAndReused(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateCA(dir, "pve1", t0)
	require.NoError(t, err)
	again, err := LoadOrCreateCA(dir, "pve1", t0.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, first.CertPEM(), again.CertPEM(), "an existing CA is reused, not replaced")

	info, err := os.Stat(filepath.Join(dir, "ca.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestClientCertificateIsBoundToTheProjectUID(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir(), "pve1", t0)
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "uid-1")

	_, err = ca.IssueClient(dir, "uid-1", t0)
	require.NoError(t, err)
	cert := readCert(t, filepath.Join(dir, "client.crt"))
	assert.Equal(t, "uid-1", cert.Subject.CommonName)
	assert.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, cert.ExtKeyUsage)
	require.NoError(t, cert.CheckSignatureFrom(ca.cert))
	ca2, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	require.NoError(t, err)
	assert.Equal(t, ca.CertPEM(), ca2)

	info, err := os.Stat(filepath.Join(dir, "client.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	before, err := os.ReadFile(filepath.Join(dir, "client.crt"))
	require.NoError(t, err)
	_, err = ca.IssueClient(dir, "uid-1", t0.Add(24*time.Hour))
	require.NoError(t, err)
	after, err := os.ReadFile(filepath.Join(dir, "client.crt"))
	require.NoError(t, err)
	assert.Equal(t, before, after, "a valid identity is kept on reissue")

	_, err = ca.IssueClient(dir, "", t0)
	assert.Error(t, err, "an identity needs a UID")
}

func TestCertificateIsRenewedNearExpiryOrForAnotherCN(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir(), "pve1", t0)
	require.NoError(t, err)
	dir := t.TempDir()
	_, err = ca.IssueClient(dir, "uid-1", t0)
	require.NoError(t, err)
	original := readCert(t, filepath.Join(dir, "client.crt"))

	nearExpiry := original.NotAfter.Add(-renewBefore + time.Hour)
	_, err = ca.IssueClient(dir, "uid-1", nearExpiry)
	require.NoError(t, err)
	renewed := readCert(t, filepath.Join(dir, "client.crt"))
	assert.NotEqual(t, original.SerialNumber, renewed.SerialNumber)

	// A directory reused for another lifetime gets that lifetime's identity.
	_, err = ca.IssueClient(dir, "uid-2", nearExpiry)
	require.NoError(t, err)
	assert.Equal(t, "uid-2", readCert(t, filepath.Join(dir, "client.crt")).Subject.CommonName)
}

func TestCertificateFromAnotherCAIsReplaced(t *testing.T) {
	other, err := LoadOrCreateCA(t.TempDir(), "pve2", t0)
	require.NoError(t, err)
	ca, err := LoadOrCreateCA(t.TempDir(), "pve1", t0)
	require.NoError(t, err)
	dir := t.TempDir()
	_, err = other.IssueClient(dir, "uid-1", t0)
	require.NoError(t, err)

	_, err = ca.IssueClient(dir, "uid-1", t0)
	require.NoError(t, err)
	assert.NoError(t, readCert(t, filepath.Join(dir, "client.crt")).CheckSignatureFrom(ca.cert))
}

// The identities work as a pair: a TLS client holding the Project's
// certificate completes a mutual handshake with the repo host's, by the alias.
func TestServerAndClientCompleteAMutualHandshake(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir(), "pve1", t0)
	require.NoError(t, err)
	serverDir, clientDir := t.TempDir(), t.TempDir()
	now := time.Now()
	_, err = ca.IssueServer(serverDir, "pve1", now)
	require.NoError(t, err)
	_, err = ca.IssueClient(clientDir, "uid-1", now)
	require.NoError(t, err)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca.CertPEM()))
	serverPair, err := tls.LoadX509KeyPair(filepath.Join(serverDir, "server.crt"), filepath.Join(serverDir, "server.key"))
	require.NoError(t, err)
	clientPair, err := tls.LoadX509KeyPair(filepath.Join(clientDir, "client.crt"), filepath.Join(clientDir, "client.key"))
	require.NoError(t, err)

	clientConn, serverConn := net.Pipe()
	done := make(chan error, 1)
	var peerCN string
	go func() {
		s := tls.Server(serverConn, &tls.Config{Certificates: []tls.Certificate{serverPair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})
		err := s.Handshake()
		if err == nil {
			peerCN = s.ConnectionState().PeerCertificates[0].Subject.CommonName
		}
		done <- err
	}()
	c := tls.Client(clientConn, &tls.Config{Certificates: []tls.Certificate{clientPair}, RootCAs: pool, ServerName: RepoAlias})
	require.NoError(t, c.Handshake())
	require.NoError(t, <-done)
	assert.Equal(t, "uid-1", peerCN)
}

func TestIssueReturnsTheFilesToHandOver(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir(), "pve1", t0)
	require.NoError(t, err)
	dir := t.TempDir()
	files, err := ca.IssueClient(dir, "uid-1", t0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key"), filepath.Join(dir, "ca.crt"),
	}, files)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "the container user can enter the directory")
}
