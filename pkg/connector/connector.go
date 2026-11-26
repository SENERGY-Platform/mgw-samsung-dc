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

// Package connector binds the tested Samsung subpackages (OCF transport, event
// mapping and command mapping) to the MGW device-connector library. One
// connector serves any number of appliances: it announces each fridge to the
// device-manager under the device id that fridge reports, polls the read
// resources on the library's schedule, each at its own configured interval, and
// writes the set commands.
//
// The services are registered once, not once per appliance: the library calls a
// registered service for every device it knows, handing it the device, and
// delivers a command together with the device id it was addressed to. The
// connector therefore keeps a table from device id to appliance and dispatches
// on it; see Connector.applianceFor.
package connector

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/SENERGY-Platform/mgw-dc-lib-go/pkg/mgw"
	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung"
	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung/mapping"
)

const (
	// ocfTimeout bounds every OCF call. pion/dtls completes its handshake lazily
	// on the first Get/Post, so without a deadline a dead appliance would block
	// the polling goroutine forever (see pkg/samsung/ocf).
	ocfTimeout = 10 * time.Second

	// ocfDeviceResource is the standard OCF device resource; ocfDeviceIdKey is its
	// device-id property and ocfDeviceNameKey the name the device reports for
	// itself. All three are OCF core, not Samsung vendor keys, so they live here
	// rather than in the samsung contract.
	ocfDeviceResource = "/oic/d"
	ocfDeviceIdKey    = "di"
	ocfDeviceNameKey  = "n"

	// defaultDeviceName labels an appliance that reports no name of its own. It
	// is qualified with the appliance's address, because a connector serving
	// several refrigerators would otherwise show the same label for all of them.
	defaultDeviceName = "Samsung Refrigerator"

	// offlineAfterFailures is the number of consecutive failed OCF reads after
	// which an appliance is announced offline. It counts failed reads, not poll
	// rounds: the library runs the read services concurrently, so one round
	// against a dead appliance contributes one failure per read service. Two
	// rounds of margin keep a single DTLS hiccup from flapping the device state.
	// It is counted per appliance, so one dead fridge never takes another
	// offline.
	offlineAfterFailures = 8

	// defaultProbeInterval is how often an offline appliance is probed for its
	// return. Register replaces it with the cadence derived from the configured
	// poll intervals, so a device is checked while offline at about the rate it
	// is polled while online.
	defaultProbeInterval = time.Minute

	// minProbeInterval is the floor under the probe cadence. The probe derives
	// its interval from the fastest read service, and that service may be
	// configured down to a few seconds; the probe runs against an appliance that
	// is already not answering, where every attempt costs a full DTLS handshake
	// into a timeout, so it is not worth running at the door-poll rate.
	minProbeInterval = 30 * time.Second

	// defaultIdRetryInterval is the wait between two attempts to read the OCF
	// device id at startup, and between two attempts to get the first device
	// update out.
	defaultIdRetryInterval = 15 * time.Second
)

// mgwClient is the part of *mgw.Client[Device] this connector uses. It exists so
// the wiring can be tested without an MQTT broker; *mgw.Client[Device] satisfies
// it as is.
type mgwClient interface {
	SetDevice(device Device) error
}

// Device is the MGW view of one appliance. The id is carried on DeviceInfo
// (json:"-") and goes out as device_id in the device update; the struct is never
// marshalled itself, so the tag here is only documentary.
//
// Online carries the reachability the connector derives from that appliance's
// OCF reads. The library hands the Device value it was last given to every
// service function and skips devices that report Offline, so this field is both
// what the SENERGY UI shows and the switch that stops polling a dead appliance —
// one appliance at a time, not the whole connector.
type Device struct {
	Id           string `json:"-"`
	Name         string
	DeviceTypeId string
	Online       bool
}

// GetInfo satisfies mgw.Device.
func (d Device) GetInfo() mgw.DeviceInfo {
	state := mgw.Offline
	if d.Online {
		state = mgw.Online
	}
	name := d.Name
	if name == "" {
		name = defaultDeviceName
	}
	return mgw.DeviceInfo{
		Id:         d.Id,
		Name:       name,
		State:      state,
		DeviceType: d.DeviceTypeId,
	}
}

// Connector holds the appliances and dispatches the registered services to
// them.
type Connector struct {
	deviceTypeId string
	logger       *slog.Logger

	// probeInterval and idRetryInterval are handed to every appliance when it
	// starts; Register derives probeInterval from the configuration and the
	// tests shorten both.
	probeInterval   time.Duration
	idRetryInterval time.Duration

	mux sync.RWMutex
	// appliances is every configured appliance in configuration order,
	// including those that have not been announced yet.
	appliances []*appliance
	// byDevice maps the OCF device id to the appliance answering for it. It is
	// filled as each appliance resolves its id, which is the only moment the id
	// is known.
	byDevice map[string]*appliance
}

// New builds a connector with no appliances; add them with Add.
func New(deviceTypeId string, logger *slog.Logger) *Connector {
	return &Connector{
		deviceTypeId:    deviceTypeId,
		logger:          logger,
		probeInterval:   defaultProbeInterval,
		idRetryInterval: defaultIdRetryInterval,
		byDevice:        map[string]*appliance{},
	}
}

// Add registers one appliance reached through ocfClient at host. It touches
// neither the network nor the broker; Start does both.
func (c *Connector) Add(ocfClient samsung.OCFClient, host string) {
	c.mux.Lock()
	defer c.mux.Unlock()
	c.appliances = append(c.appliances, &appliance{
		ocf:          ocfClient,
		host:         host,
		logger:       c.logger,
		deviceTypeId: c.deviceTypeId,
	})
}

// readServices binds every polled read service to the OCF resource it reads and
// the mapping that turns the representation into the event. It is the
// registration order, and it is checked against samsung.ReadServices in the
// tests so a service added to the contract cannot stay unregistered here.
var readServices = []struct {
	localId     string
	path        string
	mapResource func(map[string]any) any
}{
	{samsung.SvcTemperatures, samsung.ResTemperatures, mapTemperatures},
	{samsung.SvcDoors, samsung.ResDoors, mapDoors},
	{samsung.SvcEnergy, samsung.ResEnergy, mapEnergy},
	{samsung.SvcRefrigeration, samsung.ResRefrigeration, mapRefrigeration},
	{samsung.SvcIcemaker, samsung.ResIcemaker, mapIcemaker},
}

// commandResources are the resources whose last read representation a command
// is built against. The appliance keeps a reading of each of them, and only of
// them: every other read is mapped to its event and dropped.
//
// It is a literal rather than a fold over mapping.ReadResourceFor so that a set
// service whose command gains a representation cannot start silently writing
// against a resource nobody polls. The test checks it against both the set
// services and the polled read services.
var commandResources = map[string]bool{
	samsung.ResTemperatures: true,
	samsung.ResIcemaker:     true,
}

// setServices are the command-driven services, in registration order.
var setServices = []string{
	samsung.SvcSetRapidFridge,
	samsung.SvcSetRapidFreezing,
	samsung.SvcSetFridgeSetpoint,
	samsung.SvcSetFreezerSetpoint,
	samsung.SvcSetIcemaker,
}

// Register registers the polled read services and the command-driven set
// services on client. Each read service carries its own interval from intervals
// so the library emits its events on its own schedule; set services carry a nil
// duration and run only when a command arrives.
//
// Each service is registered once for the whole connector, not once per
// appliance: the library calls it for every device it knows and passes the
// device, which is what the handlers dispatch on.
//
// A read service missing from intervals, or carrying a non-positive duration, is
// registered without a schedule: it is then reachable as a request but emits no
// events. The configuration fills an entry for every service, so this is only
// reachable from a caller that builds the map itself.
func (c *Connector) Register(client *mgw.Client[Device], intervals map[string]time.Duration) {
	c.mux.Lock()
	c.probeInterval = probeIntervalFor(intervals)
	c.mux.Unlock()

	for _, r := range readServices {
		// One duration value per service: the library keeps the pointer and
		// dereferences it on every tick, so the services must not share one.
		d := intervals[r.localId]
		var schedule *time.Duration
		if d > 0 {
			schedule = &d
		}
		client.RegisterService(r.localId, c.read(r.path, r.mapResource), schedule)
	}

	for _, svc := range setServices {
		client.RegisterService(svc, c.write(svc), nil)
	}
}

// probeIntervalFor derives the offline-probe cadence from the configured read
// intervals: the fastest one, so an appliance that comes back is noticed about
// as quickly as the fastest service would have noticed it, but never below
// minProbeInterval. With no positive interval configured it falls back to
// defaultProbeInterval rather than leaving the probe unscheduled, because the
// probe is the only thing that reads an offline device at all.
func probeIntervalFor(intervals map[string]time.Duration) time.Duration {
	fastest := time.Duration(0)
	for _, d := range intervals {
		if d > 0 && (fastest == 0 || d < fastest) {
			fastest = d
		}
	}
	switch {
	case fastest == 0:
		return defaultProbeInterval
	case fastest < minProbeInterval:
		return minProbeInterval
	default:
		return fastest
	}
}

// Start brings every configured appliance up, each in its own goroutine, and
// returns at once. An appliance that is unreachable keeps retrying in the
// background until it answers or ctx ends; it holds back neither the connector
// nor the other appliances, which is the whole reason the announce is not done
// here synchronously.
//
// Call it after Register, so an appliance that announces immediately finds its
// services in place.
func (c *Connector) Start(ctx context.Context, client mgwClient) {
	c.mux.RLock()
	appliances := append([]*appliance(nil), c.appliances...)
	probeInterval, idRetryInterval := c.probeInterval, c.idRetryInterval
	c.mux.RUnlock()

	for _, a := range appliances {
		a.probeInterval = probeInterval
		a.idRetryInterval = idRetryInterval
		go a.run(ctx, client, c.bind)
	}
}

// bind makes a answer for the device id it resolved. It reports false when
// another appliance already holds that id, which happens when one refrigerator
// is reachable under two configured addresses. Announcing it twice would give
// the device-manager two devices with one id whose online state and events
// overwrite each other, so the second appliance is dropped instead.
func (c *Connector) bind(id string, a *appliance) bool {
	c.mux.Lock()
	defer c.mux.Unlock()
	if _, taken := c.byDevice[id]; taken {
		return false
	}
	c.byDevice[id] = a
	return true
}

// applianceFor resolves the appliance a service call is meant for. The library
// passes the device it polls or the device a command was addressed to, so the
// id is the dispatch key.
func (c *Connector) applianceFor(deviceId string) (*appliance, error) {
	c.mux.RLock()
	defer c.mux.RUnlock()
	a, ok := c.byDevice[deviceId]
	if !ok {
		return nil, fmt.Errorf("no appliance for device id %q", deviceId)
	}
	return a, nil
}

// Refresh re-announces every known device in its current state. The MGW client
// calls it when the device-manager asks every connector to report its devices
// again (e.g. after a device-manager restart). Appliances that have not been
// announced yet are skipped; their own announce is still pending.
func (c *Connector) Refresh() {
	c.mux.RLock()
	appliances := make([]*appliance, 0, len(c.byDevice))
	for _, a := range c.byDevice {
		appliances = append(appliances, a)
	}
	c.mux.RUnlock()

	for _, a := range appliances {
		a.refresh()
	}
}

// read builds a polled read handler for one resource. The library marshals the
// result and sends it as the service's event of the device it was called for.
func (c *Connector) read(path string, mapResource func(map[string]any) any) mgw.ServiceFunc[Device] {
	return func(d Device, _ interface{}) (interface{}, error) {
		a, err := c.applianceFor(d.Id)
		if err != nil {
			return nil, err
		}
		return a.read(path, mapResource)
	}
}

// write builds a command handler for one set service.
func (c *Connector) write(serviceLocalId string) mgw.ServiceFunc[Device] {
	return func(d Device, input interface{}) (interface{}, error) {
		a, err := c.applianceFor(d.Id)
		if err != nil {
			return nil, err
		}
		return a.write(serviceLocalId, input)
	}
}

// The read mappers adapt the contract's typed mapping functions to the
// interface-valued shape read expects. They are the only glue between the mapping
// package's concrete event structs and the library's json.Marshal of the result.
func mapTemperatures(res map[string]any) any  { return mapping.MapTemperatures(res) }
func mapDoors(res map[string]any) any         { return mapping.MapDoors(res) }
func mapEnergy(res map[string]any) any        { return mapping.MapEnergy(res) }
func mapRefrigeration(res map[string]any) any { return mapping.MapRefrigeration(res) }
func mapIcemaker(res map[string]any) any      { return mapping.MapIcemaker(res) }
