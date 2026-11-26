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

package connector

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung"
	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung/mapping"
)

// appliance is one refrigerator: its transport, the device it was announced as,
// and the reachability state derived from its reads. Every field that used to
// be connector-wide lives here, because two appliances share nothing but the
// configuration — one of them going offline must leave the other polling.
type appliance struct {
	ocf    samsung.OCFClient
	host   string
	logger *slog.Logger

	deviceTypeId    string
	probeInterval   time.Duration
	idRetryInterval time.Duration

	mux    sync.Mutex
	client mgwClient
	device *Device
	// failures counts consecutive failed OCF reads; a successful read resets it.
	failures int
	// announcing is set while a device update is on its way to the broker. The
	// update is sent without a.mux held, so this is what keeps a second reader
	// from sending the same transition twice in the meantime; the reader that
	// finds it set does nothing, and the next read decides again on the state
	// the first one left behind.
	announcing bool
	// representations holds the last read representation of every resource a
	// set service is built against, keyed by resource path; commandResources
	// names them. A command is built against the appliance's own last reading,
	// so a write addresses the item that appliance reported and is checked
	// against the range that appliance reported — never another one's.
	representations map[string]map[string]any
}

// run brings one appliance up and keeps it there: it waits for the OCF device
// id, hands the appliance to bind so commands and events for that id reach it,
// announces it online and starts the offline probe. It returns when the
// appliance is running or ctx ends.
//
// bind runs before the announce. The library delivers commands and polls the
// device as soon as SetDevice returns, so an appliance that is not yet in the
// dispatch table at that moment would answer its first command with "unknown
// device". A bind that is refused — another appliance already answers for this
// id — ends the appliance instead: see Connector.bind.
func (a *appliance) run(ctx context.Context, client mgwClient, bind func(string, *appliance) bool) {
	id, name, err := a.resolveDevice(ctx)
	if err != nil {
		a.logger.Warn("giving up on appliance", "host", a.host, "error", err)
		return
	}
	if !bind(id, a) {
		// An error, not a warning: this appliance stays unserved until the
		// connector is restarted with the duplicate entry removed, and the
		// configuration check cannot catch the case, because the same fridge
		// reached once by name and once by address is two distinct endpoints.
		a.logger.Error("appliance ignored, another one already answers for this device id",
			"device_id", id, "host", a.host)
		return
	}
	device := Device{Id: id, Name: name, DeviceTypeId: a.deviceTypeId, Online: true}
	if !a.announceInitial(ctx, client, device) {
		return
	}
	a.logger.Info("announced device",
		"device_id", id, "name", name, "host", a.host, "device_type_id", a.deviceTypeId)
	go a.probeWhileOffline(ctx)
}

// announceInitial sends the first device update and retries it until it is out
// or ctx ends. The update travels over MQTT, which is routinely not connected
// yet in the seconds after start; dropping the appliance on that first error
// would lose it until the next restart of the whole connector, including the
// appliances that did get through.
func (a *appliance) announceInitial(ctx context.Context, client mgwClient, device Device) bool {
	for attempt := 1; ; attempt++ {
		a.mux.Lock()
		a.client = client
		a.device = &device
		a.failures = 0
		a.mux.Unlock()

		err := client.SetDevice(device)
		if err == nil {
			return true
		}
		a.logger.Warn("could not announce device, retrying",
			"device_id", device.Id, "host", a.host,
			"attempt", attempt, "retry_in", a.idRetryInterval.String(), "error", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(a.idRetryInterval):
		}
	}
}

// refresh re-announces the known device in its current state. It is a no-op
// until the appliance has been announced.
//
// The update goes out without a.mux held; see announceState for why.
func (a *appliance) refresh() {
	a.mux.Lock()
	client, device := a.client, a.device
	a.mux.Unlock()
	if client == nil || device == nil {
		return
	}
	if err := client.SetDevice(*device); err != nil {
		a.logger.Warn("re-announce on refresh failed", "host", a.host, "error", err)
	}
}

// read fetches one OCF resource under a bounded context and returns the mapped
// event. Every call feeds the reachability tracking, which is the only signal
// the appliance gives about being alive.
func (a *appliance) read(path string, mapResource func(map[string]any) any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ocfTimeout)
	defer cancel()
	res, err := a.ocf.Get(ctx, path)
	a.recordRead(err)
	if err != nil {
		return nil, err
	}
	if commandResources[path] {
		a.storeRepresentation(path, res)
	}
	return mapResource(res), nil
}

// write turns the command input into an OCF write via the command mapping and
// posts it under a bounded context.
//
// A failed write does not count towards the reachability tracking: a command can
// fail because the appliance rejects it, and the polled reads are the signal
// that says whether it is reachable at all.
func (a *appliance) write(serviceLocalId string, input any) (any, error) {
	representation, err := a.commandContext(serviceLocalId)
	if err != nil {
		return nil, err
	}
	path, body, err := mapping.BuildCommandFromRepresentation(serviceLocalId, input, representation)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), ocfTimeout)
	defer cancel()
	if err := a.ocf.Post(ctx, path, body); err != nil {
		return nil, err
	}
	return nil, nil
}

// storeRepresentation keeps the last representation of one resource for the
// commands built against it. The stored map is only ever read afterwards — the
// transport decodes a fresh map per read and the mapping does not write to it —
// so a command may read it while the next poll round is already running.
func (a *appliance) storeRepresentation(path string, res map[string]any) {
	a.mux.Lock()
	defer a.mux.Unlock()
	if a.representations == nil {
		a.representations = make(map[string]map[string]any, len(commandResources))
	}
	a.representations[path] = res
}

// commandContext returns the representation a command is built against: the
// last reading of the resource mapping.ReadResourceFor names, nil for a service
// that needs none.
//
// A command before the first successful read of that resource is rejected
// rather than written without a representation. Without one the mapping can
// neither address the item by the id the device reported nor check the value
// against the minimum and maximum the device itself reported, so the write
// would carry an unchecked value to a guessed description — and an unchecked
// setpoint is exactly what must not reach a fridge. The library runs the first
// poll round immediately after Register, so the rejection window is one poll at
// startup.
func (a *appliance) commandContext(serviceLocalId string) (map[string]any, error) {
	path := mapping.ReadResourceFor(serviceLocalId)
	if path == "" {
		return nil, nil
	}
	a.mux.Lock()
	defer a.mux.Unlock()
	res, ok := a.representations[path]
	if !ok {
		return nil, fmt.Errorf("no reading of %s yet, the appliance has not reported what the command writes into", path)
	}
	return res, nil
}

// recordRead folds the outcome of one OCF read into the reachability state. The
// appliance is announced offline after offlineAfterFailures consecutive
// failures and online again on the first success.
func (a *appliance) recordRead(err error) {
	a.mux.Lock()
	if a.client == nil || a.device == nil {
		a.mux.Unlock()
		return
	}
	var change bool
	var online bool
	if err == nil {
		a.failures = 0
		change, online = !a.device.Online, true
	} else {
		a.failures++
		change, online = a.device.Online && a.failures >= offlineAfterFailures, false
	}
	if !change || a.announcing {
		a.mux.Unlock()
		return
	}
	device := *a.device
	device.Online = online
	client, failures := a.client, a.failures
	a.announcing = true
	a.mux.Unlock()

	a.announceState(client, device, failures)
}

// announceState sends one reachability change to the device-manager. It runs
// without a.mux held, because SetDevice subscribes and publishes over MQTT and
// the library waits for both without a deadline of its own: holding the lock
// across that would stall every read of this appliance, and through the
// library's per-service wait the next poll round of the others with it.
//
// The stored device is only updated once the update is out, so a failed
// announce leaves the state as it was and the next read with the same outcome
// sends it again.
func (a *appliance) announceState(client mgwClient, device Device, failures int) {
	err := client.SetDevice(device)

	a.mux.Lock()
	a.announcing = false
	if err == nil {
		a.device = &device
	}
	a.mux.Unlock()

	if err != nil {
		a.logger.Warn("could not announce device state",
			"device_id", device.Id, "state", string(device.GetInfo().State), "error", err)
		return
	}
	a.logger.Info("device state changed",
		"device_id", device.Id, "state", string(device.GetInfo().State),
		"host", a.host, "failed_reads", failures)
}

// probeWhileOffline brings a device that was reported offline back online. The
// library stops polling a device in state Offline, so without this probe the
// reads that would notice the appliance's return never run again.
func (a *appliance) probeWhileOffline(ctx context.Context) {
	ticker := time.NewTicker(a.probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.online() {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, ocfTimeout)
			_, err := a.ocf.Get(cctx, ocfDeviceResource)
			cancel()
			if err != nil {
				a.logger.Debug("offline probe failed",
					"resource", ocfDeviceResource, "host", a.host, "error", err)
				continue
			}
			a.recordRead(nil)
		}
	}
}

func (a *appliance) online() bool {
	a.mux.Lock()
	defer a.mux.Unlock()
	return a.device != nil && a.device.Online
}

// resolveDevice reads the OCF device id and name from /oic/d and retries until
// it succeeds or ctx ends.
//
// There is deliberately no fallback id: an id derived from anything but the OCF
// di would announce a second device as soon as /oic/d is readable again, which
// splits the measurement series and leaves the first device standing online
// forever. An appliance that is unreachable at startup waits here instead,
// which is why this runs per appliance and not before the connector is up: a
// refrigerator that is switched off must not hold back the one next to it.
func (a *appliance) resolveDevice(ctx context.Context) (id string, name string, err error) {
	for attempt := 1; ; attempt++ {
		id, name, err = a.readDevice(ctx)
		if err == nil {
			return id, name, nil
		}
		a.logger.Warn("could not read ocf device id, retrying",
			"resource", ocfDeviceResource, "host", a.host,
			"attempt", attempt, "retry_in", a.idRetryInterval.String(), "error", err)
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("resolve device id from %s: %w", ocfDeviceResource, ctx.Err())
		case <-time.After(a.idRetryInterval):
		}
	}
}

// readDevice reads the device id and the display name from /oic/d. The name is
// what the appliance calls itself; a connector serving several refrigerators
// would otherwise announce all of them under one label. An appliance that
// reports no name falls back to the model label plus its address, which is
// still unique per appliance.
func (a *appliance) readDevice(ctx context.Context) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, ocfTimeout)
	defer cancel()
	res, err := a.ocf.Get(cctx, ocfDeviceResource)
	if err != nil {
		return "", "", err
	}
	di, ok := res[ocfDeviceIdKey].(string)
	if !ok || di == "" {
		return "", "", fmt.Errorf("%s carries no %q", ocfDeviceResource, ocfDeviceIdKey)
	}
	return di, a.deviceName(res), nil
}

// deviceName is the label the device is announced under: the name the appliance
// reports under /oic/d, or the model label with the appliance's address.
func (a *appliance) deviceName(res map[string]any) string {
	if n, ok := res[ocfDeviceNameKey].(string); ok && n != "" {
		return n
	}
	return defaultDeviceName + " (" + a.host + ")"
}
