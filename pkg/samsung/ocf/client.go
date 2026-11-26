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

// OCF-over-CoAP/DTLS transport for the mgw-samsung-dc device connector. It
// implements the samsung.OCFClient interface against one appliance.
//
// Third-party libraries (all permissive OSS):
//   - github.com/plgd-dev/go-coap/v3  (Apache-2.0) — CoAP, incl. the DTLS transport
//   - github.com/pion/dtls/v3         (MIT)        — DTLS carrier for go-coap
//   - github.com/fxamacker/cbor/v2    (MIT)        — CBOR codec for OCF representations
//
// Note: go-coap v3 binds its DTLS transport to pion/dtls/v3 (not v2); the
// transport's *dtls.Config must be the v3 type, so this package uses v3.

package ocf

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/fxamacker/cbor/v2"

	piondtls "github.com/pion/dtls/v3"
	coapdtls "github.com/plgd-dev/go-coap/v3/dtls"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	coapclient "github.com/plgd-dev/go-coap/v3/udp/client"

	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung"
)

// DefaultSecurePort is the OCF/CoAPS (CoAP-over-DTLS) port. It is configurable
// via the port argument to New; this is only the default a caller may pass.
const DefaultSecurePort = 49155

// ocfContentFormat is the CoAP Content-Format for the request/response bodies.
// OCF 1.x mandates application/vnd.ocf+cbor (10000). The plan fixed the CBOR
// *codec* (fxamacker) but not this wire option, and 10000 is the OCF-correct
// value for an OCF appliance.
//
// Confirmed against the live TP1X_REF_21K: 10000 is accepted and the resources
// decode. Should another model answer 4.15 (Unsupported Content-Format), switch
// this single value to message.AppCBOR (60, application/cbor); decoding does not
// depend on it, the response body is CBOR-decoded regardless of the format the
// device reports. (message.AppOcfCbor is a package var, hence var here, not const.)
var ocfContentFormat = message.AppOcfCbor

// ocfDecMode decodes OCF representations so that every CBOR map — including maps
// nested inside arrays such as x.com.samsung.da.items — becomes a
// map[string]any. Without DefaultMapType, fxamacker decodes maps nested in an
// interface{} to map[any]any, which would make the event mapping's
// item.(map[string]any) assertions fail. The contract treats OCF resources as
// map[string]any throughout, so this is the faithful decode.
var ocfDecMode cbor.DecMode = func() cbor.DecMode {
	dm, err := cbor.DecOptions{DefaultMapType: reflect.TypeOf(map[string]any(nil))}.DecMode()
	if err != nil {
		panic(fmt.Sprintf("ocf: build cbor decode mode: %v", err))
	}
	return dm
}()

// Client is an OCF/CoAP/DTLS transport to one appliance. It satisfies
// samsung.OCFClient.
//
// The DTLS session is established lazily, per call, and is dropped as soon as it
// is known to be unusable. It must be, because the appliance is a household
// device on the user's LAN: it is routinely unreachable when the connector
// starts (power cut, reboot, Wi-Fi not up yet) and comes back minutes later. A
// session dialed once at startup does not survive that — go-coap closes the
// connection after the failed handshake, and every later request on it fails
// with "use of closed network connection" until the process is restarted.
type Client struct {
	addr string
	cert tls.Certificate
	uuid string

	// mu guards conn. It is held while dialing, so two concurrent calls open at
	// most one session, and released before the request itself, so a slow
	// request never blocks another.
	mu   sync.Mutex
	conn *coapclient.Conn
}

// compile-time proof that Client implements the contract.
var _ samsung.OCFClient = (*Client)(nil)

// New builds an OCF/CoAP/DTLS client for host:port. It touches the network not
// at all; the first Get or Post dials.
//
// certDir holds the connector's persistent client identity (see cert.go): the
// ECDSA key and self-signed certificate it presents to the appliance, created
// there on first use and re-read on every later start. The appliance's own
// certificate is self-signed on the user's local LAN; its chain is not locally
// trusted (InsecureSkipVerify), which is intentional for this local device —
// there is no CA to validate against and no hostname to match.
//
// clientUUID is the UUID the certificate presents; the appliance authorizes the
// client by that string, not by the key. An empty clientUUID derives the UUID
// from the key instead (generic OCF).
func New(host string, port int, certDir string, clientUUID string) (*Client, error) {
	cert, uuid, err := loadOrCreateClientCert(certDir, clientUUID)
	if err != nil {
		return nil, fmt.Errorf("ocf: client certificate: %w", err)
	}
	return &Client{
		addr: net.JoinHostPort(host, strconv.Itoa(port)),
		cert: cert,
		uuid: uuid,
	}, nil
}

// session returns a usable DTLS session, dialing once if there is none or the
// previous one has died. One call dials at most once: a failed dial is returned
// to the caller, which turns one unreachable device into one failed request
// rather than a retry loop inside it.
//
// The DTLS handshake itself is lazy (pion/dtls v3 completes it on first
// Read/Write, like crypto/tls), so a successful dial is not yet a reachable
// device; the handshake failure surfaces on the request below.
//
// It does not surface under that request's context, though, and no context
// passed to Get or Post bounds it: go-coap drives the handshake from its own
// read loop under context.Background() while holding pion's handshake mutex, so
// a request waits on that mutex regardless of its own deadline. A peer that
// answers — including with an ICMP rejection, the case this transport recovers
// from — ends the handshake promptly either way. A peer that silently drops
// packets does not, and pion retransmits without a cap, so the call blocks until
// the peer answers or the session is closed.
func (c *Client) session() (*coapclient.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		if alive(c.conn) {
			return c.conn, nil
		}
		_ = c.conn.Close()
		c.conn = nil
	}

	// A fresh config per dial: pion takes the pointer as given, and sharing one
	// config across sessions would couple them for no gain.
	//
	// GetClientCertificate forces our certificate into the handshake. The
	// appliance sends a CertificateRequest carrying AcceptableCAs, and pion's
	// default certificate selection sends a certificate only when one is issued
	// by one of those CAs. Ours is self-signed and matches none, so the default
	// would send an empty certificate: the handshake still completes (server
	// auth only, which InsecureSkipVerify waives), but the session is then
	// unauthenticated and the appliance drops every ACL-guarded request while
	// still answering the public resources. The appliance authorizes by the
	// "uuid:" string in the certificate subject, not by its chain, so returning
	// the certificate unconditionally is what the device actually checks.
	conn, err := coapdtls.Dial(c.addr, &piondtls.Config{
		Certificates:       []tls.Certificate{c.cert},
		InsecureSkipVerify: true, // local self-signed device cert: no chain to trust, no hostname to match
		GetClientCertificate: func(*piondtls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &c.cert, nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ocf: dial %s: %w", c.addr, err)
	}
	c.conn = conn
	return conn, nil
}

// drop discards conn so the next call dials a new session. It is a no-op for a
// session that has already been replaced, so a late failure from an old session
// cannot tear down the current one.
func (c *Client) drop(conn *coapclient.Conn) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
	_ = conn.Close()
}

// alive reports whether a session can still carry requests. go-coap closes the
// connection's context when its run loop ends, which is what a failed handshake
// or a dropped session looks like from here.
func alive(conn *coapclient.Conn) bool {
	select {
	case <-conn.Done():
		return false
	default:
		return conn.Context().Err() == nil
	}
}

// dead reports whether err from a request means the session itself is finished,
// as opposed to one request going wrong. A request that merely ran out of its
// caller's time keeps a session that is still up.
func dead(conn *coapclient.Conn, err error) bool {
	if err == nil {
		return false
	}
	if !alive(conn) {
		return true
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// pion reports a failed DTLS handshake as a wrapped error whose message
	// carries "handshake"; it has no exported sentinel to match on.
	return strings.Contains(err.Error(), "handshake")
}

// ClientUUID returns the OCF device UUID derived from the client certificate.
func (c *Client) ClientUUID() string {
	return c.uuid
}

// Get reads one OCF resource and decodes its CBOR representation to a map.
func (c *Client) Get(ctx context.Context, path string) (map[string]any, error) {
	v, err := c.GetValue(ctx, path)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return map[string]any{}, nil
	}
	out, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("ocf: get %s: representation is %T, not a map", path, v)
	}
	return out, nil
}

// GetValue reads one OCF resource and decodes its CBOR representation without
// requiring a map. Resource representations are maps, which is what the
// contract's Get returns and what every consumer wants; the discovery resource
// /oic/res is an array of links, so reading it needs the undecided shape. A
// resource with an empty body decodes to nil.
func (c *Client) GetValue(ctx context.Context, path string) (any, error) {
	conn, err := c.session()
	if err != nil {
		return nil, err
	}

	resp, err := conn.Get(ctx, path)
	if err != nil {
		if dead(conn, err) {
			c.drop(conn)
		}
		return nil, fmt.Errorf("ocf: get %s: %w", path, err)
	}
	if !isSuccess(resp.Code()) {
		return nil, fmt.Errorf("ocf: get %s: unexpected response code %s", path, resp.Code())
	}

	body, err := resp.ReadBody()
	if err != nil {
		return nil, fmt.Errorf("ocf: get %s: read body: %w", path, err)
	}
	if len(body) == 0 {
		return nil, nil
	}

	var out any
	if err := ocfDecMode.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("ocf: get %s: decode cbor: %w", path, err)
	}
	return out, nil
}

// Post writes a CBOR-encoded body to one OCF resource (OCF UPDATE).
func (c *Client) Post(ctx context.Context, path string, body map[string]any) error {
	payload, err := cbor.Marshal(body)
	if err != nil {
		return fmt.Errorf("ocf: post %s: encode cbor: %w", path, err)
	}

	conn, err := c.session()
	if err != nil {
		return err
	}

	resp, err := conn.Post(ctx, path, ocfContentFormat, bytes.NewReader(payload))
	if err != nil {
		if dead(conn, err) {
			c.drop(conn)
		}
		return fmt.Errorf("ocf: post %s: %w", path, err)
	}
	if !isSuccess(resp.Code()) {
		return fmt.Errorf("ocf: post %s: unexpected response code %s", path, resp.Code())
	}
	return nil
}

// Close releases the current DTLS session, if any. A later Get or Post would
// dial again; shutdown is the caller's business, not this type's.
func (c *Client) Close() error {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()

	if conn == nil {
		return nil
	}
	return conn.Close()
}

// isSuccess reports whether a CoAP response code is in the 2.xx success class
// (Created, Deleted, Valid, Changed, Content). CoAP encodes the class in the
// high three bits of the code byte.
func isSuccess(code codes.Code) bool {
	return code>>5 == 2
}
