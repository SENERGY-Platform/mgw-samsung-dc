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

package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung"
)

// env builds a getenv function backed by a map, so a test never touches the
// process environment.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadRequiresConnectorId(t *testing.T) {
	_, err := load(env(map[string]string{"OCF_HOST": "192.168.0.5"}))
	if err == nil {
		t.Fatal("expected an error when CONNECTOR_ID is missing")
	}
}

func TestLoadRequiresOcfHost(t *testing.T) {
	for _, value := range []string{"", "  ", ",", " , "} {
		_, err := load(env(map[string]string{
			"CONNECTOR_ID": "mgw-samsung-dc",
			"OCF_HOST":     value,
		}))
		if err == nil {
			t.Errorf("OCF_HOST %q names no appliance and must be rejected", value)
		}
	}
}

// TestLoadParsesApplianceList: one connector serves every appliance OCF_HOST
// lists, each on its own port or on OCF_PORT.
func TestLoadParsesApplianceList(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"CONNECTOR_ID": "mgw-samsung-dc",
		"OCF_HOST":     " 192.168.0.5, fridge.local:49156 ,, [fe80::1]:49157 , fe80::2 ",
		"OCF_PORT":     "49155",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Appliance{
		{Host: "192.168.0.5", Port: 49155},
		{Host: "fridge.local", Port: 49156},
		{Host: "fe80::1", Port: 49157},
		{Host: "fe80::2", Port: 49155},
	}
	if !reflect.DeepEqual(cfg.Appliances, want) {
		t.Errorf("Appliances = %+v, want %+v", cfg.Appliances, want)
	}
	if got, want := cfg.Appliances[2].Addr(), "[fe80::1]:49157"; got != want {
		t.Errorf("Addr = %q, want %q", got, want)
	}
}

// TestLoadSingleHostIsOneAppliance: the value every deployment had before the
// list existed still means exactly one appliance.
func TestLoadSingleHostIsOneAppliance(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"CONNECTOR_ID": "mgw-samsung-dc",
		"OCF_HOST":     "192.168.0.5",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Appliance{{Host: "192.168.0.5", Port: 49155}}
	if !reflect.DeepEqual(cfg.Appliances, want) {
		t.Errorf("Appliances = %+v, want %+v", cfg.Appliances, want)
	}
}

// TestLoadRejectsDuplicateAppliance: two entries for one endpoint mean a typo
// in one of them, and starting with one device would hide it.
func TestLoadRejectsDuplicateAppliance(t *testing.T) {
	_, err := load(env(map[string]string{
		"CONNECTOR_ID": "mgw-samsung-dc",
		"OCF_HOST":     "192.168.0.5, 192.168.0.5:49155",
	}))
	if err == nil {
		t.Fatal("a duplicate endpoint must be rejected")
	}
}

func TestLoadRejectsInvalidAppliancePort(t *testing.T) {
	for _, value := range []string{
		"192.168.0.5:abc",
		"192.168.0.5:0",
		"192.168.0.5:70000",
		":49155",
		"192.168.0.5:",    // a colon without a port
		"192.168.0.5:+80", // a sign is a typo, not a port
		"[fe80::1]:",
	} {
		_, err := load(env(map[string]string{
			"CONNECTOR_ID": "mgw-samsung-dc",
			"OCF_HOST":     value,
		}))
		if err == nil {
			t.Errorf("OCF_HOST %q must be rejected", value)
		}
	}
}

// TestLoadRejectsMistypedApplianceList: an entry that is not a host must be
// refused at startup. It would otherwise pass as a hostname, and the connector
// would start, announce nothing for it and say so only in a retry warning.
func TestLoadRejectsMistypedApplianceList(t *testing.T) {
	for _, value := range []string{
		"192.168.0.5 192.168.0.6", // a space instead of the comma
		"192.168.0.5;192.168.0.6", // a semicolon instead of the comma
		"192.168.0.5:49156:1",     // a port appended twice
		"192.168.0.5:49156:49157", // and again
		"[192.168.0.5",            // half a bracket
	} {
		_, err := load(env(map[string]string{
			"CONNECTOR_ID": "mgw-samsung-dc",
			"OCF_HOST":     value,
		}))
		if err == nil {
			t.Errorf("OCF_HOST %q is not a host and must be rejected", value)
		}
	}
}

// TestLoadAcceptsIPv6Forms: an IPv6 address is written in brackets when it
// carries a port, and usually in brackets even when it does not. Both reach the
// transport as the bare address, which is what net.JoinHostPort expects.
func TestLoadAcceptsIPv6Forms(t *testing.T) {
	cases := map[string]Appliance{
		"fe80::1":         {Host: "fe80::1", Port: 49155},
		"[fe80::1]":       {Host: "fe80::1", Port: 49155},
		"[fe80::1]:49156": {Host: "fe80::1", Port: 49156},
		"fe80::1%eth0":    {Host: "fe80::1%eth0", Port: 49155},
		"[fe80::1%eth0]":  {Host: "fe80::1%eth0", Port: 49155},
	}
	for value, want := range cases {
		cfg, err := load(env(map[string]string{
			"CONNECTOR_ID": "mgw-samsung-dc",
			"OCF_HOST":     value,
		}))
		if err != nil {
			t.Errorf("OCF_HOST %q: unexpected error: %v", value, err)
			continue
		}
		if got := cfg.Appliances[0]; got != want {
			t.Errorf("OCF_HOST %q = %+v, want %+v", value, got, want)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"CONNECTOR_ID": "mgw-samsung-dc",
		"OCF_HOST":     "192.168.0.5",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OcfPort != 49155 {
		t.Errorf("OcfPort default = %d, want 49155", cfg.OcfPort)
	}
	if len(cfg.Appliances) != 1 || cfg.Appliances[0].Port != 49155 {
		t.Errorf("Appliances = %+v, want one appliance on the default port", cfg.Appliances)
	}
	if cfg.PollInterval != time.Minute {
		t.Errorf("PollInterval default = %s, want 1m0s", cfg.PollInterval)
	}
	if cfg.Debug {
		t.Error("Debug default = true, want false")
	}
	if cfg.MgwMqttBroker != "tcp://localhost:1883" {
		t.Errorf("MgwMqttBroker default = %q, want tcp://localhost:1883", cfg.MgwMqttBroker)
	}
	if cfg.DeviceTypeId != "" {
		t.Errorf("DeviceTypeId default = %q, want empty", cfg.DeviceTypeId)
	}
	if cfg.CertPath != "/data/client-cert" {
		t.Errorf("CertPath default = %q, want /data/client-cert", cfg.CertPath)
	}
	if cfg.OcfClientUUID != "ab0b0ac4-aae9-4958-a04d-8ec36fe1b2f9" {
		t.Errorf("OcfClientUUID default = %q, want the Samsung service UUID", cfg.OcfClientUUID)
	}
}

func TestLoadParsesValues(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"CONNECTOR_ID":    "mgw-samsung-dc",
		"OCF_HOST":        "fridge.local",
		"OCF_PORT":        "5555",
		"POLL_INTERVAL":   "30s",
		"DEBUG":           "true",
		"DEVICE_TYPE_ID":  "urn:infai:ses:device-type:abc",
		"MGW_MQTT_BROKER": "tcp://broker:1881",
		"MGW_MQTT_USER":   "user",
		"MGW_MQTT_PW":     "secret",
		"CERT_PATH":       "/var/lib/mgw-samsung-dc/cert",
		"OCF_CLIENT_UUID": "11111111-2222-3333-4444-555555555555",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OcfPort != 5555 {
		t.Errorf("OcfPort = %d, want 5555", cfg.OcfPort)
	}
	if got, want := (Appliance{Host: "fridge.local", Port: 5555}), cfg.Appliances[0]; got != want {
		t.Errorf("Appliances[0] = %+v, want %+v", want, got)
	}
	if cfg.PollInterval != 30*time.Second {
		t.Errorf("PollInterval = %s, want 30s", cfg.PollInterval)
	}
	if !cfg.Debug {
		t.Error("Debug = false, want true")
	}
	if cfg.CertPath != "/var/lib/mgw-samsung-dc/cert" {
		t.Errorf("CertPath = %q, want /var/lib/mgw-samsung-dc/cert", cfg.CertPath)
	}
	if cfg.OcfClientUUID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("OcfClientUUID = %q, want the overridden value", cfg.OcfClientUUID)
	}
	if cfg.DeviceTypeId != "urn:infai:ses:device-type:abc" {
		t.Errorf("DeviceTypeId = %q", cfg.DeviceTypeId)
	}
	if cfg.MgwMqttBroker != "tcp://broker:1881" {
		t.Errorf("MgwMqttBroker = %q", cfg.MgwMqttBroker)
	}
	if cfg.MgwMqttUser != "user" || cfg.MgwMqttPw != "secret" {
		t.Errorf("MgwMqttUser/Pw = %q/%q", cfg.MgwMqttUser, cfg.MgwMqttPw)
	}
}

func TestLoadRejectsInvalidPollInterval(t *testing.T) {
	_, err := load(env(map[string]string{
		"CONNECTOR_ID":  "mgw-samsung-dc",
		"OCF_HOST":      "fridge.local",
		"POLL_INTERVAL": "not-a-duration",
	}))
	if err == nil {
		t.Fatal("expected an error for an unparseable POLL_INTERVAL")
	}
}

func TestLoadRejectsNonPositivePollInterval(t *testing.T) {
	_, err := load(env(map[string]string{
		"CONNECTOR_ID":  "mgw-samsung-dc",
		"OCF_HOST":      "fridge.local",
		"POLL_INTERVAL": "0s",
	}))
	if err == nil {
		t.Fatal("expected an error for a non-positive POLL_INTERVAL")
	}
}

func TestLoadRejectsInvalidOcfPort(t *testing.T) {
	_, err := load(env(map[string]string{
		"CONNECTOR_ID": "mgw-samsung-dc",
		"OCF_HOST":     "fridge.local",
		"OCF_PORT":     "70000",
	}))
	if err == nil {
		t.Fatal("expected an error for an out-of-range OCF_PORT")
	}
}

func TestLoadPollIntervalsDefaultToPollInterval(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"CONNECTOR_ID":  "mgw-samsung-dc",
		"OCF_HOST":      "192.168.0.5",
		"POLL_INTERVAL": "45s",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.PollIntervals) != len(samsung.ReadServices) {
		t.Fatalf("PollIntervals has %d entries, want one per read service (%d)",
			len(cfg.PollIntervals), len(samsung.ReadServices))
	}
	for _, svc := range samsung.ReadServices {
		if got := cfg.PollIntervals[svc]; got != 45*time.Second {
			t.Errorf("PollIntervals[%q] = %s, want 45s", svc, got)
		}
	}
}

func TestLoadPollIntervalOverridePerService(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"CONNECTOR_ID":                "mgw-samsung-dc",
		"OCF_HOST":                    "192.168.0.5",
		"POLL_INTERVAL":               "60s",
		"POLL_INTERVAL_DOORS":         "10s",
		"POLL_INTERVAL_REFRIGERATION": "5m",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]time.Duration{
		samsung.SvcDoors:         10 * time.Second,
		samsung.SvcRefrigeration: 5 * time.Minute,
		samsung.SvcTemperatures:  time.Minute,
		samsung.SvcEnergy:        time.Minute,
	}
	for svc, w := range want {
		if got := cfg.PollIntervals[svc]; got != w {
			t.Errorf("PollIntervals[%q] = %s, want %s", svc, got, w)
		}
	}
	// The override must not move the shared default.
	if cfg.PollInterval != time.Minute {
		t.Errorf("PollInterval = %s, want 1m0s", cfg.PollInterval)
	}
}

func TestLoadRejectsInvalidServicePollInterval(t *testing.T) {
	_, err := load(env(map[string]string{
		"CONNECTOR_ID":        "mgw-samsung-dc",
		"OCF_HOST":            "192.168.0.5",
		"POLL_INTERVAL_DOORS": "soon",
	}))
	if err == nil {
		t.Fatal("expected an error for an unparsable POLL_INTERVAL_DOORS")
	}
	if !strings.Contains(err.Error(), "POLL_INTERVAL_DOORS") {
		t.Errorf("error %q does not name the offending variable", err)
	}
}

func TestLoadRejectsNonPositiveServicePollInterval(t *testing.T) {
	_, err := load(env(map[string]string{
		"CONNECTOR_ID":         "mgw-samsung-dc",
		"OCF_HOST":             "192.168.0.5",
		"POLL_INTERVAL_ENERGY": "0s",
	}))
	if err == nil {
		t.Fatal("expected an error for a non-positive POLL_INTERVAL_ENERGY")
	}
}
