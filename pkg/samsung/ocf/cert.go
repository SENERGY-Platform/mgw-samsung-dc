/*
 * Copyright (c) 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Client certificate and OCF device identity for the OCF/CoAP/DTLS transport.

package ocf

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// File names of the persisted client identity inside the certificate directory,
// and the permissions they are created with.
const (
	keyFileName  = "client.key"
	certFileName = "client.crt"

	// keyFileMode keeps the private key readable by its owner only. certDirMode
	// is just as restrictive, so on a shared volume the key is not merely
	// unreadable for others but its directory is not listable either.
	keyFileMode  = fs.FileMode(0o600)
	certFileMode = fs.FileMode(0o644)
	certDirMode  = fs.FileMode(0o700)
)

// loadOrCreateClientCert returns the connector's persistent client certificate
// and the OCF device UUID it carries, creating dir and the identity on first
// use.
//
// wantUUID is the UUID the certificate must present. It is the appliance's
// authorization anchor: the device matches the client by the "uuid:" string in
// the certificate subject, not by the key, so the subject must carry exactly
// the UUID the device's access control list grants. An empty wantUUID falls
// back to a UUID derived from the key (generic OCF, where the client owns its
// own identity).
//
// The key is persistent either way: it is written once and only re-read
// afterwards, so the TLS identity is stable across restarts. A certificate that
// is missing, does not belong to the key, or carries a different UUID than
// wantUUID is re-issued from the existing key — re-keying would be pointless
// here (the device ignores the key) and for the generic path would silently
// change the identity.
func loadOrCreateClientCert(dir string, wantUUID string) (tls.Certificate, string, error) {
	if dir == "" {
		return tls.Certificate{}, "", errors.New("certificate directory is empty")
	}
	if err := os.MkdirAll(dir, certDirMode); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("create certificate directory %s: %w", dir, err)
	}

	keyPath := filepath.Join(dir, keyFileName)
	certPath := filepath.Join(dir, certFileName)

	priv, err := readKey(keyPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return tls.Certificate{}, "", fmt.Errorf("generate ecdsa key: %w", err)
		}
		if err := writeKey(keyPath, priv); err != nil {
			return tls.Certificate{}, "", err
		}
	case err != nil:
		return tls.Certificate{}, "", err
	}

	uuid := wantUUID
	if uuid == "" {
		uuid, err = deriveDeviceUUID(&priv.PublicKey)
		if err != nil {
			return tls.Certificate{}, "", fmt.Errorf("derive device uuid: %w", err)
		}
	}

	der, err := readCert(certPath, &priv.PublicKey, uuid)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return tls.Certificate{}, "", err
	}
	if err != nil {
		cert, _, cerr := issueClientCert(priv, uuid)
		if cerr != nil {
			return tls.Certificate{}, "", cerr
		}
		der = cert.Certificate[0]
		if werr := writeCert(certPath, der); werr != nil {
			return tls.Certificate{}, "", werr
		}
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("parse certificate %s: %w", certPath, err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, uuid, nil
}

// readKey loads the PKCS#8 PEM private key at path. A missing file is reported
// as fs.ErrNotExist so the caller can create one; any other problem is an error,
// never a silent re-key.
func readKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("read private key %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("private key %s is not a PEM PRIVATE KEY block", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key %s: %w", path, err)
	}
	priv, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key %s is %T, want *ecdsa.PrivateKey", path, key)
	}
	return priv, nil
}

// writeKey stores priv as a PKCS#8 PEM file readable only by its owner. It
// refuses to overwrite an existing file: an existing key is the identity, and
// replacing it is never the right recovery.
func writeKey(path string, priv *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
	if err != nil {
		return fmt.Errorf("create private key %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	// O_CREATE applies the mode through the umask, so set it explicitly; a
	// umask of 0 would otherwise be the only way to be sure of 0600.
	if err := f.Chmod(keyFileMode); err != nil {
		return fmt.Errorf("chmod private key %s: %w", path, err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		return fmt.Errorf("write private key %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close private key %s: %w", path, err)
	}
	return nil
}

// readCert loads the PEM certificate at path and returns its DER bytes. A
// certificate that does not belong to pub, or does not present wantUUID in its
// subject, is reported as fs.ErrNotExist, so the caller re-issues it. The UUID
// check is what rewrites a certificate from an earlier identity when the
// configured UUID changes, even though the key is unchanged.
func readCert(path string, pub *ecdsa.PublicKey, wantUUID string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("read certificate %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("certificate %s is not a PEM CERTIFICATE block", path)
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate %s: %w", path, err)
	}
	certPub, ok := parsed.PublicKey.(*ecdsa.PublicKey)
	if !ok || !certPub.Equal(pub) {
		return nil, fs.ErrNotExist
	}
	if parsed.Subject.CommonName != subjectCommonName(wantUUID) {
		return nil, fs.ErrNotExist
	}
	return block.Bytes, nil
}

// writeCert stores the DER certificate as PEM. Unlike the key it may be
// replaced, because it carries no secret and is re-derivable from the key.
func writeCert(path string, der []byte) error {
	buf := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if buf == nil {
		return fmt.Errorf("encode certificate %s: pem encoding failed", path)
	}
	if err := os.WriteFile(path, buf, certFileMode); err != nil {
		return fmt.Errorf("write certificate %s: %w", path, err)
	}
	return nil
}

// ocfNamespace is the UUIDv5 namespace used to derive the OCF device UUID from
// the client public key. It is the RFC 4122 X.500 DN namespace
// (6ba7b812-9dad-11d1-80b4-00c04fd430c8); the public key is an X.509/X.500
// artifact, so deriving the identity in that namespace is the natural choice.
var ocfNamespace = [16]byte{
	0x6b, 0xa7, 0xb8, 0x12, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// generateClientCert creates a fresh self-signed ECDSA P-256 client certificate
// and returns it together with the OCF device UUID derived from its public key.
//
// It is the in-memory path, used where no identity is persisted. The connector
// itself goes through loadOrCreateClientCert, because a key that only lives for
// one process makes the connector a different device after every restart.
//
// This is the generic-OCF path, used only when no UUID is configured (see
// loadOrCreateClientCert). The UUID is UUIDv5 over the DER-encoded
// SubjectPublicKeyInfo of the client key, in the X.500 namespace above,
// deterministic in the key. The supported Samsung appliances do not take a
// derived UUID — they authorize a fixed service UUID — so against them the
// configured UUID is used instead; confirmed live that the subject must carry
// the "uuid:" form, which subjectCommonName provides.
func generateClientCert() (tls.Certificate, string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate ecdsa key: %w", err)
	}

	uuid, err := deriveDeviceUUID(&priv.PublicKey)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("derive device uuid: %w", err)
	}
	return issueClientCert(priv, uuid)
}

// subjectCommonName is the certificate common name for an OCF device UUID. The
// "urn:uuid:" prefix is what the appliance matches on: TizenRT iotivity locates
// the peer UUID with memmem(subject, "uuid:"), so a bare UUID in the subject is
// never found, however correct the UUID itself is.
func subjectCommonName(uuid string) string {
	return "urn:uuid:" + uuid
}

// issueClientCert self-signs a client certificate for an existing key. The
// device UUID goes into the common name, the organizational unit and a URI SAN,
// each carrying the "uuid:" substring the appliance matches on.
func issueClientCert(priv *ecdsa.PrivateKey, uuid string) (tls.Certificate, string, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate serial: %w", err)
	}

	sanURI, err := url.Parse(subjectCommonName(uuid))
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("build san uri: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         subjectCommonName(uuid),
			OrganizationalUnit: []string{"uuid:" + uuid},
		},
		URIs:                  []*url.URL{sanURI},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("create certificate: %w", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("parse created certificate: %w", err)
	}

	cert := tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
		Leaf:        leaf,
	}
	return cert, uuid, nil
}

// deriveDeviceUUID returns the OCF device UUID for a client public key, as
// UUIDv5 over the key's DER-encoded SubjectPublicKeyInfo. It is deterministic:
// the same key always yields the same UUID.
func deriveDeviceUUID(pub *ecdsa.PublicKey) (string, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	return uuidV5(ocfNamespace, spki), nil
}

// uuidV5 computes an RFC 4122 version-5 (SHA-1, name-based) UUID and formats it
// as the canonical lowercase 8-4-4-4-12 string.
func uuidV5(namespace [16]byte, name []byte) string {
	h := sha1.New()
	h.Write(namespace[:])
	h.Write(name)
	sum := h.Sum(nil) // 20 bytes; the UUID uses the first 16

	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant

	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
