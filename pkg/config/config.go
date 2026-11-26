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

// Package config loads the connector configuration from environment variables.
//
// connectlife loads its config with go-service-base/config-hdl, but that module
// is not a dependency of this connector; pulling it in only to read a handful of
// env vars would add three modules (config-hdl, go-env-loader, the types
// subpackage). The plan allows the stdlib fallback when config-hdl would be a new
// dependency, so this reads os.Getenv directly and parses the few typed values by
// hand. That also keeps the test hermetic: load takes the getenv function, so a
// test never touches the process environment.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung"
)

// Config holds the connector configuration. All values come from environment
// variables; CONNECTOR_ID and OCF_HOST are required, the rest have defaults.
type Config struct {
	ConnectorId   string // CONNECTOR_ID (required)
	MgwMqttBroker string // MGW_MQTT_BROKER
	MgwMqttUser   string // MGW_MQTT_USER
	MgwMqttPw     string // MGW_MQTT_PW
	// Appliances are the refrigerators this connector serves, in the order
	// OCF_HOST lists them. One process serves all of them: the MGW library
	// registers every service once and calls it per device, so a second
	// appliance costs a second OCF session, not a second deployment.
	Appliances []Appliance
	// OcfPort is the port of an OCF_HOST entry that names none (OCF_PORT,
	// default 49155). The parsed per-appliance port is on Appliance.Port; this
	// is only the fallback it was filled from.
	OcfPort      int           // OCF_PORT (default 49155)
	PollInterval time.Duration // POLL_INTERVAL (default 60s)
	// PollIntervals is the poll interval per read service, keyed by the service
	// local id. It holds an entry for every samsung.ReadServices entry:
	// PollInterval, unless POLL_INTERVAL_<SERVICE> overrides it. The services
	// differ in how fast their value changes and in how fast a stale value
	// matters, so a single interval either polls a door too slowly or the rapid
	// modes far too often.
	PollIntervals map[string]time.Duration
	Debug         bool   // DEBUG (default false)
	DeviceTypeId  string // DEVICE_TYPE_ID (SENERGY device type id; may be empty)
	// OcfClientUUID is the UUID the OCF client certificate presents to the
	// appliance (OCF_CLIENT_UUID). The appliance authorizes the client by this
	// string in the certificate subject, not by the key, so it must be a UUID
	// the appliance's access control list grants. It defaults to the Samsung
	// service identity that the factory ACL of these refrigerators already
	// grants full access to. OCF_CLIENT_UUID overrides it; an empty value (only
	// reachable by code, e.g. the CLI) derives the UUID from the key instead
	// (generic OCF, requires onboarding the derived UUID on the device).
	OcfClientUUID string
	// CertPath is the directory holding the OCF client key and certificate
	// (CERT_PATH, default /data/client-cert). It must be on a volume that
	// survives container restarts: the derived device UUID is the connector's
	// identity towards the appliance, and the appliance authorizes it once.
	CertPath string
}

// Appliance is one refrigerator: the address of its OCF endpoint. Everything
// else — device type, poll intervals, client certificate — is shared by every
// appliance of one connector, because they are the same model on the same LAN
// served by the same client identity.
type Appliance struct {
	Host string
	Port int
}

// Addr is the appliance's OCF endpoint as host:port, with an IPv6 host in
// brackets.
func (a Appliance) Addr() string {
	return net.JoinHostPort(a.Host, strconv.Itoa(a.Port))
}

const (
	defaultMgwMqttBroker = "tcp://localhost:1883"
	// defaultOcfPort mirrors ocf.DefaultSecurePort; it is duplicated as a literal
	// so this package does not import the DTLS transport just for one number.
	defaultOcfPort      = 49155
	defaultPollInterval = time.Minute
	defaultCertPath     = "/data/client-cert"
	// defaultOcfClientUUID is the Samsung service identity the factory access
	// control list of the supported refrigerators grants perm=31 on href=*. The
	// appliance matches the client by this UUID string in the certificate
	// subject, so presenting it is what authorizes the reads. It is geräte-
	// übergreifend constant; it is overridden only for a device that authorizes
	// a different identity.
	defaultOcfClientUUID = "ab0b0ac4-aae9-4958-a04d-8ec36fe1b2f9"
)

// Load reads the configuration from the process environment.
func Load() (*Config, error) {
	return load(os.Getenv)
}

// load reads the configuration via getenv. It is the hermetic core of Load and
// exists so tests can supply a fake environment.
func load(getenv func(string) string) (*Config, error) {
	cfg := &Config{
		MgwMqttBroker: defaultMgwMqttBroker,
		OcfPort:       defaultOcfPort,
		PollInterval:  defaultPollInterval,
		CertPath:      defaultCertPath,
		OcfClientUUID: defaultOcfClientUUID,
	}

	cfg.ConnectorId = getenv("CONNECTOR_ID")
	if cfg.ConnectorId == "" {
		return nil, fmt.Errorf("config: CONNECTOR_ID is required")
	}
	if v := getenv("MGW_MQTT_BROKER"); v != "" {
		cfg.MgwMqttBroker = v
	}
	cfg.MgwMqttUser = getenv("MGW_MQTT_USER")
	cfg.MgwMqttPw = getenv("MGW_MQTT_PW")

	if v := getenv("OCF_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: OCF_PORT %q is not a valid integer: %w", v, err)
		}
		if p <= 0 || p > 65535 {
			return nil, fmt.Errorf("config: OCF_PORT %d is out of range 1-65535", p)
		}
		cfg.OcfPort = p
	}

	// After OCF_PORT: an entry without its own port falls back to it.
	appliances, err := parseAppliances(getenv("OCF_HOST"), cfg.OcfPort)
	if err != nil {
		return nil, err
	}
	cfg.Appliances = appliances

	if v := getenv("POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("config: POLL_INTERVAL %q is not a valid duration: %w", v, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("config: POLL_INTERVAL must be positive, got %s", d)
		}
		cfg.PollInterval = d
	}

	cfg.PollIntervals = make(map[string]time.Duration, len(samsung.ReadServices))
	for _, svc := range samsung.ReadServices {
		d := cfg.PollInterval
		name := pollIntervalVar(svc)
		if v := getenv(name); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("config: %s %q is not a valid duration: %w", name, v, err)
			}
			if parsed <= 0 {
				return nil, fmt.Errorf("config: %s must be positive, got %s", name, parsed)
			}
			d = parsed
		}
		cfg.PollIntervals[svc] = d
	}

	if v := getenv("DEBUG"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("config: DEBUG %q is not a valid boolean: %w", v, err)
		}
		cfg.Debug = b
	}

	cfg.DeviceTypeId = getenv("DEVICE_TYPE_ID")

	if v := getenv("CERT_PATH"); v != "" {
		cfg.CertPath = v
	}

	if v := getenv("OCF_CLIENT_UUID"); v != "" {
		cfg.OcfClientUUID = v
	}

	return cfg, nil
}

// pollIntervalVar is the environment variable overriding the poll interval of
// one read service: the service local id upper-cased, with a POLL_INTERVAL_
// prefix. "refrigeration" becomes POLL_INTERVAL_REFRIGERATION.
func pollIntervalVar(serviceLocalId string) string {
	return "POLL_INTERVAL_" + strings.ToUpper(serviceLocalId)
}

// parseAppliances turns the OCF_HOST value into the list of appliances to
// serve. It is a comma-separated list, because that is what the module
// definition's isList config produces: "10.0.0.5" is one appliance,
// "10.0.0.5, 10.0.0.6:49156" is two, the second on its own port. An entry
// without a port gets defaultPort.
//
// A duplicate endpoint is an error rather than a silently collapsed list: two
// entries for one address mean the operator meant two appliances and mistyped
// one, and starting with one device would hide that until somebody misses the
// measurements of the other.
func parseAppliances(hosts string, defaultPort int) ([]Appliance, error) {
	appliances := make([]Appliance, 0, 1)
	seen := make(map[string]bool)
	for _, entry := range strings.Split(hosts, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		a, err := parseAppliance(entry, defaultPort)
		if err != nil {
			return nil, err
		}
		if seen[a.Addr()] {
			return nil, fmt.Errorf("config: OCF_HOST lists %s twice", a.Addr())
		}
		seen[a.Addr()] = true
		appliances = append(appliances, a)
	}
	if len(appliances) == 0 {
		return nil, fmt.Errorf("config: OCF_HOST is required")
	}
	return appliances, nil
}

// parseAppliance reads one OCF_HOST entry, "host" or "host:port".
//
// A bare IPv6 address carries colons itself, so an entry that does not split
// into host and port is taken as a host in full — but only if it is one. The
// entries come from a text field in the module UI, where a list is easily typed
// with the wrong separator ("10.0.0.5 10.0.0.6") or a port appended twice
// ("10.0.0.5:49156:1"). Both of those split into nothing and would otherwise
// pass as hostnames, leaving a connector that starts clean and silently never
// announces the device: the only sign would be a retry warning every fifteen
// seconds. They are rejected here instead.
func parseAppliance(entry string, defaultPort int) (Appliance, error) {
	host, port := entry, defaultPort
	if h, p, err := net.SplitHostPort(entry); err == nil {
		if p == "" {
			return Appliance{}, fmt.Errorf("config: OCF_HOST entry %q ends in a colon without a port", entry)
		}
		n, perr := parsePort(p)
		if perr != nil {
			return Appliance{}, fmt.Errorf("config: OCF_HOST entry %q has no valid port: %w", entry, perr)
		}
		host, port = h, n
	} else {
		// "[fe80::1]" is how an IPv6 address is usually written down, and
		// SplitHostPort rejects it for the missing port. The brackets are the
		// notation, not part of the address — but only as a pair, so that a
		// half-typed "[10.0.0.5" is not quietly repaired into a host.
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		}
	}
	host = strings.TrimSpace(host)
	if !validHost(host) {
		return Appliance{}, fmt.Errorf("config: OCF_HOST entry %q is not a host; "+
			"separate several appliances with a comma and write an IPv6 address with a port as [addr]:port", entry)
	}
	return Appliance{Host: host, Port: port}, nil
}

// parsePort reads a port, digits only. strconv.Atoi alone would also take a
// sign, and "+49156" in a config field is a typo, not a port.
func parsePort(s string) (int, error) {
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a number", s)
		}
	}
	p, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if p <= 0 || p > 65535 {
		return 0, fmt.Errorf("port %d is out of range 1-65535", p)
	}
	return p, nil
}

// validHost reports whether host can be dialled: a hostname or an IP address,
// with an optional zone on an IPv6 address. Only an IPv6 address may carry a
// colon, which is what separates a real address from an entry that lost its
// separator.
func validHost(host string) bool {
	if host == "" || strings.ContainsAny(host, " \t\r\n[]") {
		return false
	}
	if !strings.Contains(host, ":") {
		return isHostname(host)
	}
	addr := host
	if i := strings.Index(addr, "%"); i >= 0 {
		addr = addr[:i] // "fe80::1%eth0": the zone is not part of the address
	}
	return net.ParseIP(addr) != nil
}

// isHostname reports whether s holds only the characters a hostname or an IPv4
// address is written with. It is a character check, not a validation of the
// name: it exists to catch an entry whose separator went wrong
// ("10.0.0.5;10.0.0.6"), not to second-guess the resolver.
func isHostname(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
