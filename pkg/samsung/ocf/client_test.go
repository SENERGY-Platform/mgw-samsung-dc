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

// Round-trip tests for the OCF/CoAP/DTLS transport against a loopback go-coap
// DTLS server. No real device is contacted.

package ocf

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	piondtls "github.com/pion/dtls/v3"
	coapdtls "github.com/plgd-dev/go-coap/v3/dtls"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/mux"
	coapnet "github.com/plgd-dev/go-coap/v3/net"
	"github.com/plgd-dev/go-coap/v3/options"
)

const testResourcePath = "/temperatures/vs/0"

var uuidV5Pattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestGenerateClientCert(t *testing.T) {
	cert, uuid, err := generateClientCert()
	if err != nil {
		t.Fatalf("generateClientCert: %v", err)
	}

	if len(cert.Certificate) == 0 {
		t.Fatal("certificate chain is empty")
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("certificate is not parseable: %v", err)
	}
	if _, ok := cert.PrivateKey.(*ecdsa.PrivateKey); !ok {
		t.Fatalf("private key is not ECDSA, got %T", cert.PrivateKey)
	}

	if uuid == "" {
		t.Fatal("derived UUID is empty")
	}
	if !uuidV5Pattern.MatchString(uuid) {
		t.Fatalf("derived UUID %q is not a canonical UUIDv5", uuid)
	}
	if parsed.Subject.CommonName != subjectCommonName(uuid) {
		t.Fatalf("certificate CN %q does not carry derived UUID %q", parsed.Subject.CommonName, uuid)
	}
}

func TestDeriveDeviceUUIDIsStable(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	first, err := deriveDeviceUUID(&priv.PublicKey)
	if err != nil {
		t.Fatalf("deriveDeviceUUID (first): %v", err)
	}
	second, err := deriveDeviceUUID(&priv.PublicKey)
	if err != nil {
		t.Fatalf("deriveDeviceUUID (second): %v", err)
	}

	if first != second {
		t.Fatalf("UUID derivation is not stable: %q != %q", first, second)
	}
	if !uuidV5Pattern.MatchString(first) {
		t.Fatalf("derived UUID %q is not a canonical UUIDv5", first)
	}

	// A different key must yield a different UUID.
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	otherUUID, err := deriveDeviceUUID(&other.PublicKey)
	if err != nil {
		t.Fatalf("deriveDeviceUUID (other): %v", err)
	}
	if otherUUID == first {
		t.Fatal("two distinct keys derived the same UUID")
	}
}

func TestGetPostRoundTrip(t *testing.T) {
	getRepresentation := map[string]any{
		"x.com.samsung.da.items": []any{
			map[string]any{
				"x.com.samsung.da.description": "fridge",
				"x.com.samsung.da.current":     4.0,
				"x.com.samsung.da.desired":     3.0,
			},
		},
		"x.com.samsung.da.rapidFridge": "Off",
	}

	received := make(chan map[string]any, 1)

	handler := func(w mux.ResponseWriter, r *mux.Message) {
		switch r.Code() {
		case codes.GET:
			payload, err := cbor.Marshal(getRepresentation)
			if err != nil {
				t.Errorf("server: marshal get representation: %v", err)
				_ = w.SetResponse(codes.InternalServerError, message.TextPlain, nil)
				return
			}
			if err := w.SetResponse(codes.Content, ocfContentFormat, bytes.NewReader(payload)); err != nil {
				t.Errorf("server: set get response: %v", err)
			}
		case codes.POST:
			body, err := r.ReadBody()
			if err != nil {
				t.Errorf("server: read post body: %v", err)
				_ = w.SetResponse(codes.BadRequest, message.TextPlain, nil)
				return
			}
			var got map[string]any
			if err := cbor.Unmarshal(body, &got); err != nil {
				t.Errorf("server: decode post body: %v", err)
				_ = w.SetResponse(codes.BadRequest, message.TextPlain, nil)
				return
			}
			received <- got
			if err := w.SetResponse(codes.Changed, ocfContentFormat, nil); err != nil {
				t.Errorf("server: set post response: %v", err)
			}
		default:
			_ = w.SetResponse(codes.MethodNotAllowed, message.TextPlain, nil)
		}
	}

	host, port := startLoopbackServer(t, handler)

	client, err := New(host, port, t.TempDir(), "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if client.ClientUUID() == "" {
		t.Fatal("client UUID is empty")
	}

	// GET round-trip: the decoded representation must equal what the server sent,
	// with every nested map a map[string]any (not map[any]any).
	ctxGet, cancelGet := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelGet()
	gotGet, err := client.Get(ctxGet, testResourcePath)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(gotGet, getRepresentation) {
		t.Fatalf("Get mismatch:\n got  %#v\n want %#v", gotGet, getRepresentation)
	}

	// POST round-trip: the server must read back exactly the map we sent.
	postBody := map[string]any{
		"x.com.samsung.da.desired":     2.0,
		"x.com.samsung.da.rapidFridge": "On",
	}
	ctxPost, cancelPost := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelPost()
	if err := client.Post(ctxPost, testResourcePath, postBody); err != nil {
		t.Fatalf("Post: %v", err)
	}

	select {
	case gotPost := <-received:
		if !reflect.DeepEqual(gotPost, postBody) {
			t.Fatalf("Post mismatch:\n got  %#v\n want %#v", gotPost, postBody)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive the POST body")
	}
}

// startLoopbackServer brings up a loopback DTLS server on a free port.
func startLoopbackServer(t *testing.T, handler mux.HandlerFunc) (string, int) {
	t.Helper()
	return startLoopbackServerAt(t, "127.0.0.1:0", handler)
}

// startLoopbackServerAt brings up a go-coap DTLS server on addr with its own
// self-signed ECDSA certificate and the given handler registered at
// testResourcePath. It requires (but does not verify) a client certificate, so
// the client's presented certificate is exercised. It returns the bound host and
// port and registers cleanup.
func startLoopbackServerAt(t *testing.T, addr string, handler mux.HandlerFunc) (string, int) {
	t.Helper()

	serverCert := selfSignedServerCert(t)
	cfg := &piondtls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   piondtls.RequireAnyClientCert, // demand a client cert, do not verify its chain
	}

	listener, err := coapnet.NewDTLSListener("udp", addr, cfg)
	if err != nil {
		t.Fatalf("new dtls listener: %v", err)
	}

	router := mux.NewRouter()
	if err := router.Handle(testResourcePath, handler); err != nil {
		t.Fatalf("register handler: %v", err)
	}

	server := coapdtls.NewServer(options.WithMux(router))
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(listener)
	}()

	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
		<-serveDone
	})

	udpAddr, ok := listener.Addr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("listener address is not *net.UDPAddr, got %T", listener.Addr())
	}
	return "127.0.0.1", udpAddr.Port
}

func selfSignedServerCert(t *testing.T) tls.Certificate {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate server serial: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mgw-samsung-dc test device"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// TestGetReconnectsAfterUnreachableDevice is the regression test for the defect
// that a device which is down when the connector starts stays unreachable
// forever: the single session dialed at startup is closed by go-coap after the
// failed handshake, and every later request on it fails with "use of closed
// network connection" until the process restarts.
func TestGetReconnectsAfterUnreachableDevice(t *testing.T) {
	representation := map[string]any{"x.com.samsung.da.rapidFridge": "Off"}

	handler := func(w mux.ResponseWriter, r *mux.Message) {
		payload, err := cbor.Marshal(representation)
		if err != nil {
			t.Errorf("server: marshal representation: %v", err)
			_ = w.SetResponse(codes.InternalServerError, message.TextPlain, nil)
			return
		}
		if err := w.SetResponse(codes.Content, ocfContentFormat, bytes.NewReader(payload)); err != nil {
			t.Errorf("server: set response: %v", err)
		}
	}

	port := reservePort(t)

	client, err := New("127.0.0.1", port, t.TempDir(), "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// Nothing is listening yet: the first call must fail, by whatever route the
	// platform reports an unreachable UDP peer.
	ctxDown, cancelDown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDown()
	if _, err := client.Get(ctxDown, testResourcePath); err == nil {
		t.Fatal("Get against an unreachable device returned no error")
	}

	// The device comes back on the same address, as a rebooting appliance does.
	startLoopbackServerAt(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), handler)

	ctxUp, cancelUp := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelUp()
	got, err := client.Get(ctxUp, testResourcePath)
	if err != nil {
		t.Fatalf("Get after the device came back: %v", err)
	}
	if !reflect.DeepEqual(got, representation) {
		t.Fatalf("Get mismatch:\n got  %#v\n want %#v", got, representation)
	}
}

// TestClientCertPersistsAcrossRestarts pins the connector's identity: the
// appliance authorizes one UUID once, so a restart must present the same one.
func TestClientCertPersistsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()

	certA, first, err := loadOrCreateClientCert(dir, "")
	if err != nil {
		t.Fatalf("loadOrCreateClientCert (first): %v", err)
	}
	certB, second, err := loadOrCreateClientCert(dir, "")
	if err != nil {
		t.Fatalf("loadOrCreateClientCert (second): %v", err)
	}

	if first != second {
		t.Fatalf("UUID changed across restarts: %q != %q", first, second)
	}
	if !uuidV5Pattern.MatchString(first) {
		t.Fatalf("derived UUID %q is not a canonical UUIDv5", first)
	}
	if !bytes.Equal(certA.Certificate[0], certB.Certificate[0]) {
		t.Fatal("certificate was re-issued although key and certificate were present")
	}

	info, err := os.Stat(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("private key mode = %04o, want 0600", perm)
	}

	// A second directory must yield a different identity — otherwise the
	// equality above would hold for a constant just as well.
	_, other, err := loadOrCreateClientCert(t.TempDir(), "")
	if err != nil {
		t.Fatalf("loadOrCreateClientCert (other dir): %v", err)
	}
	if other == first {
		t.Fatal("two independent certificate directories produced the same UUID")
	}
}

// TestClientCertReissuedFromStoredKey covers the half-written state: the key is
// the identity, so a missing certificate is re-issued from it rather than
// triggering a re-key.
func TestClientCertReissuedFromStoredKey(t *testing.T) {
	dir := t.TempDir()

	_, first, err := loadOrCreateClientCert(dir, "")
	if err != nil {
		t.Fatalf("loadOrCreateClientCert (first): %v", err)
	}
	if err := os.Remove(filepath.Join(dir, certFileName)); err != nil {
		t.Fatalf("remove certificate: %v", err)
	}

	cert, second, err := loadOrCreateClientCert(dir, "")
	if err != nil {
		t.Fatalf("loadOrCreateClientCert (after certificate loss): %v", err)
	}
	if second != first {
		t.Fatalf("UUID changed after the certificate was lost: %q != %q", first, second)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("re-issued certificate is not parseable: %v", err)
	}
	if parsed.Subject.CommonName != subjectCommonName(first) {
		t.Fatalf("re-issued certificate CN %q does not carry UUID %q", parsed.Subject.CommonName, first)
	}
}

// TestClientCertUsesConfiguredUUID pins Weg A: a configured UUID must land in
// the certificate subject verbatim with the "uuid:" substring the appliance
// matches on, independent of the key, and a changed UUID must re-issue the
// certificate from the unchanged key.
func TestClientCertUsesConfiguredUUID(t *testing.T) {
	dir := t.TempDir()
	const wantUUID = "ab0b0ac4-aae9-4958-a04d-8ec36fe1b2f9"

	cert, got, err := loadOrCreateClientCert(dir, wantUUID)
	if err != nil {
		t.Fatalf("loadOrCreateClientCert: %v", err)
	}
	if got != wantUUID {
		t.Fatalf("returned UUID %q, want %q", got, wantUUID)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("certificate is not parseable: %v", err)
	}
	if parsed.Subject.CommonName != "urn:uuid:"+wantUUID {
		t.Fatalf("CN %q, want urn:uuid:%s", parsed.Subject.CommonName, wantUUID)
	}
	// TizenRT locates the peer UUID with memmem(subject, "uuid:"); the raw
	// DER subject must carry that substring for the match to succeed.
	if !bytes.Contains(parsed.RawSubject, []byte("uuid:"+wantUUID)) {
		t.Fatalf("certificate subject does not carry %q", "uuid:"+wantUUID)
	}
	foundSAN := false
	for _, u := range parsed.URIs {
		if u.String() == "urn:uuid:"+wantUUID {
			foundSAN = true
		}
	}
	if !foundSAN {
		t.Fatalf("certificate has no urn:uuid SAN, got %v", parsed.URIs)
	}

	// The key is the anchor across a UUID change: a second call with a different
	// UUID keeps the key but re-issues the certificate.
	keyBefore, err := os.ReadFile(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	cert2, got2, err := loadOrCreateClientCert(dir, "11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatalf("loadOrCreateClientCert (changed UUID): %v", err)
	}
	if got2 != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("returned UUID %q after change", got2)
	}
	keyAfter, err := os.ReadFile(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("read key after change: %v", err)
	}
	if !bytes.Equal(keyBefore, keyAfter) {
		t.Fatal("private key was rewritten on a UUID change")
	}
	if bytes.Equal(cert.Certificate[0], cert2.Certificate[0]) {
		t.Fatal("certificate was not re-issued on a UUID change")
	}
}

// reservePort returns a loopback UDP port that is free right now. The port is
// released before it is returned, so a test can dial it while nothing listens
// and bind it afterwards.
func reservePort(t *testing.T) int {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve udp port: %v", err)
	}
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		_ = conn.Close()
		t.Fatalf("reserved address is not *net.UDPAddr, got %T", conn.LocalAddr())
	}
	port := addr.Port
	if err := conn.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return port
}

// TestGetRedialsAfterSessionDiedWhileIdle covers the session that dies between
// two polls — an appliance rebooting while the connector waits out its poll
// interval. The next call must dial a new session instead of spending one
// request discovering that the old one is gone.
func TestGetRedialsAfterSessionDiedWhileIdle(t *testing.T) {
	representation := map[string]any{"x.com.samsung.da.rapidFridge": "On"}

	handler := func(w mux.ResponseWriter, r *mux.Message) {
		payload, err := cbor.Marshal(representation)
		if err != nil {
			t.Errorf("server: marshal representation: %v", err)
			_ = w.SetResponse(codes.InternalServerError, message.TextPlain, nil)
			return
		}
		if err := w.SetResponse(codes.Content, ocfContentFormat, bytes.NewReader(payload)); err != nil {
			t.Errorf("server: set response: %v", err)
		}
	}

	host, port := startLoopbackServer(t, handler)

	client, err := New(host, port, t.TempDir(), "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := client.Get(ctx, testResourcePath); err != nil {
		t.Fatalf("Get (first): %v", err)
	}

	// Kill the established session behind the client's back, the way go-coap
	// does when the underlying DTLS connection ends.
	client.mu.Lock()
	established := client.conn
	client.mu.Unlock()
	if established == nil {
		t.Fatal("no session was established by the first Get")
	}
	if err := established.Close(); err != nil {
		t.Fatalf("close established session: %v", err)
	}

	got, err := client.Get(ctx, testResourcePath)
	if err != nil {
		t.Fatalf("Get after the session died: %v", err)
	}
	if !reflect.DeepEqual(got, representation) {
		t.Fatalf("Get mismatch:\n got  %#v\n want %#v", got, representation)
	}
}
