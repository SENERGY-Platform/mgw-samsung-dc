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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/mgw-dc-lib-go/pkg/mgw"
	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung"
	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung/mapping"
)

// fakeOCF stands in for the DTLS transport. It answers Get from representations
// and can be switched to failing, which is how an unreachable appliance looks to
// the connector: every call returns an error.
type fakeOCF struct {
	mux sync.Mutex

	representations map[string]map[string]any
	// failUntil counts down: while > 0 every Get fails and the counter drops.
	failUntil int
	// down makes every Get fail for as long as it is set.
	down bool
	// postErr is returned by every Post.
	postErr error

	gets  map[string]int
	posts []post
}

type post struct {
	path string
	body map[string]any
}

func newFakeOCF() *fakeOCF {
	return &fakeOCF{
		representations: map[string]map[string]any{
			ocfDeviceResource: {ocfDeviceIdKey: "0c1d2e3f-aaaa-bbbb-cccc-445566778899"},
			samsung.ResDoors: {samsung.KeyItems: []any{
				map[string]any{
					samsung.KeyDescription: samsung.CompartmentFridge,
					samsung.KeyOpenState:   samsung.StateOpen,
				},
			}},
			samsung.ResTemperatures: {samsung.KeyItems: []any{
				map[string]any{
					samsung.KeyID:          uint64(0),
					samsung.KeyDescription: "Fridge",
					samsung.KeyCurrent:     4.0,
					samsung.KeyDesired:     3.0,
					samsung.KeyMinimum:     1.0,
					samsung.KeyMaximum:     7.0,
				},
			}},
			samsung.ResIcemaker: {
				samsung.KeyIceMakerState:  samsung.ValueOn,
				samsung.KeyIceMakerStatus: "ICESTATUS_STOP",
				samsung.KeyIceMakerType:   samsung.IceMakerTypeToggle,
			},
		},
		gets: map[string]int{},
	}
}

var errUnreachable = errors.New("dtls handshake timeout")

func (f *fakeOCF) Get(_ context.Context, path string) (map[string]any, error) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.gets[path]++
	if f.down {
		return nil, errUnreachable
	}
	if f.failUntil > 0 {
		f.failUntil--
		return nil, errUnreachable
	}
	res, ok := f.representations[path]
	if !ok {
		return nil, errors.New("no representation for " + path)
	}
	return res, nil
}

func (f *fakeOCF) Post(_ context.Context, path string, body map[string]any) error {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.posts = append(f.posts, post{path, body})
	return f.postErr
}

func (f *fakeOCF) Close() error { return nil }

func (f *fakeOCF) setDown(down bool) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.down = down
}

func (f *fakeOCF) getCount(path string) int {
	f.mux.Lock()
	defer f.mux.Unlock()
	return f.gets[path]
}

func (f *fakeOCF) sentPosts() []post {
	f.mux.Lock()
	defer f.mux.Unlock()
	return append([]post(nil), f.posts...)
}

// fakeMgw records every device update the connector sends.
type fakeMgw struct {
	mux sync.Mutex

	devices []Device
	// err is returned by the next failCount calls to SetDevice.
	err       error
	failCount int
	// hold blocks every SetDevice that announces an offline device until it is
	// closed, standing in for a broker that accepts the subscribe and then
	// never acknowledges. blocked counts the calls waiting on it.
	hold    chan struct{}
	blocked int
}

// blockedAnnounces is the number of device updates waiting on hold.
func (f *fakeMgw) blockedAnnounces() int {
	f.mux.Lock()
	defer f.mux.Unlock()
	return f.blocked
}

func (f *fakeMgw) SetDevice(device Device) error {
	f.mux.Lock()
	hold := f.hold
	if hold != nil && !device.Online {
		f.blocked++
	}
	f.mux.Unlock()
	if hold != nil && !device.Online {
		<-hold
		f.mux.Lock()
		f.blocked--
		f.mux.Unlock()
	}

	f.mux.Lock()
	defer f.mux.Unlock()
	if f.failCount > 0 {
		f.failCount--
		return f.err
	}
	f.devices = append(f.devices, device)
	return nil
}

// states returns the announced states in order, e.g. ["online" "offline"].
func (f *fakeMgw) states() []string {
	f.mux.Lock()
	defer f.mux.Unlock()
	states := make([]string, 0, len(f.devices))
	for _, d := range f.devices {
		states = append(states, string(d.GetInfo().State))
	}
	return states
}

// snapshot is the last announced state per device id.
func (f *fakeMgw) snapshot() map[string]Device {
	f.mux.Lock()
	defer f.mux.Unlock()
	latest := make(map[string]Device, len(f.devices))
	for _, d := range f.devices {
		latest[d.Id] = d
	}
	return latest
}

func (f *fakeMgw) last() (Device, bool) {
	f.mux.Lock()
	defer f.mux.Unlock()
	if len(f.devices) == 0 {
		return Device{}, false
	}
	return f.devices[len(f.devices)-1], true
}

const (
	testDeviceTypeId = "urn:infai:ses:device-type:fridge"
	testHost         = "192.168.1.5"
	testDeviceId     = "0c1d2e3f-aaaa-bbbb-cccc-445566778899"
)

// testConnector builds a connector with one appliance on ocf, not started. The
// retry intervals are shortened so the announce and probe loops run at test
// speed.
func testConnector(ocf samsung.OCFClient, more ...samsung.OCFClient) *Connector {
	c := New(testDeviceTypeId, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.idRetryInterval = time.Millisecond
	c.probeInterval = time.Millisecond
	c.Add(ocf, testHost)
	for i, extra := range more {
		c.Add(extra, fmt.Sprintf("192.168.1.%d", 6+i))
	}
	return c
}

// startedConnector builds a connector with one appliance, starts it and waits
// until the appliance is announced. It returns the device the library would
// hand to the service handlers, which is what they dispatch on.
func startedConnector(t *testing.T, ocf samsung.OCFClient) (*Connector, *fakeMgw, Device) {
	t.Helper()
	c := testConnector(ocf)
	m := &fakeMgw{}
	c.Start(t.Context(), m)
	waitFor(t, func() bool { _, ok := m.last(); return ok }, "no device announced")
	device, _ := m.last()
	return c, m, device
}

// TestAnnounceWaitsForOcfDeviceId covers defect 2: an unreadable /oic/d must not
// produce a second, host-derived device id. The connector retries until the OCF
// di is readable and announces that one id.
func TestAnnounceWaitsForOcfDeviceId(t *testing.T) {
	ocf := newFakeOCF()
	ocf.failUntil = 3
	c := testConnector(ocf)
	m := &fakeMgw{}

	c.Start(t.Context(), m)
	waitFor(t, func() bool { _, ok := m.last(); return ok }, "no device announced")

	if got := ocf.getCount(ocfDeviceResource); got != 4 {
		t.Errorf("expected three failed and one successful read of %s, got %d", ocfDeviceResource, got)
	}
	device, _ := m.last()
	info := device.GetInfo()
	if info.Id != testDeviceId {
		t.Errorf("device must carry the ocf di, got %q", info.Id)
	}
	if info.State != mgw.Online || info.DeviceType != testDeviceTypeId {
		t.Errorf("unexpected device info %+v", info)
	}
	if got := len(m.states()); got != 1 {
		t.Errorf("exactly one device must be announced, got %d", got)
	}
}

// TestAnnounceNamesTheAppliance: the device carries the name the appliance
// reports for itself, so several refrigerators are told apart in the UI. An
// appliance that reports none falls back to the model label and its address.
func TestAnnounceNamesTheAppliance(t *testing.T) {
	ocf := newFakeOCF()
	ocf.representations[ocfDeviceResource] = map[string]any{
		ocfDeviceIdKey:   testDeviceId,
		ocfDeviceNameKey: "Fridge Kitchen",
	}
	_, m, _ := startedConnector(t, ocf)
	device, _ := m.last()
	if got := device.GetInfo().Name; got != "Fridge Kitchen" {
		t.Errorf("device name = %q, want the name the appliance reports", got)
	}

	nameless := newFakeOCF()
	nameless.representations[ocfDeviceResource] = map[string]any{ocfDeviceIdKey: testDeviceId}
	_, m2, _ := startedConnector(t, nameless)
	device2, _ := m2.last()
	if got, want := device2.GetInfo().Name, defaultDeviceName+" ("+testHost+")"; got != want {
		t.Errorf("device name = %q, want %q", got, want)
	}
}

// TestAnnounceWaitsForOcfDeviceIdWithoutDi treats a representation without a di
// like an unreadable one: no id is invented from it.
func TestAnnounceWaitsForOcfDeviceIdWithoutDi(t *testing.T) {
	ocf := newFakeOCF()
	ocf.representations[ocfDeviceResource] = map[string]any{ocfDeviceNameKey: "Refrigerator"}
	c := testConnector(ocf)
	m := &fakeMgw{}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	c.Start(ctx, m)
	<-ctx.Done()
	waitFor(t, func() bool { return ocf.getCount(ocfDeviceResource) >= 2 },
		"the device id must be retried")
	if len(m.states()) != 0 {
		t.Errorf("nothing may be announced without a di, got %v", m.states())
	}
}

// TestAnnounceAbortsOnContextEnd: a connector shut down while the appliance is
// unreachable gives up instead of announcing anything.
func TestAnnounceAbortsOnContextEnd(t *testing.T) {
	ocf := newFakeOCF()
	ocf.setDown(true)
	c := testConnector(ocf)
	m := &fakeMgw{}
	ctx, cancel := context.WithCancel(t.Context())

	c.Start(ctx, m)
	time.Sleep(20 * time.Millisecond)
	cancel()

	if len(m.states()) != 0 {
		t.Errorf("nothing may be announced, got %v", m.states())
	}
}

// TestUnreachableApplianceDoesNotHoldBackTheOthers is the reason the announce
// runs per appliance: a refrigerator that is switched off must not keep the one
// next to it from being announced and polled.
func TestUnreachableApplianceDoesNotHoldBackTheOthers(t *testing.T) {
	dead := newFakeOCF()
	dead.setDown(true)
	alive := newFakeOCF()
	c := testConnector(dead, alive)
	m := &fakeMgw{}

	c.Start(t.Context(), m)
	waitFor(t, func() bool { _, ok := m.last(); return ok },
		"the reachable appliance must be announced while the other is down")

	device, _ := m.last()
	if device.GetInfo().Id != testDeviceId {
		t.Errorf("unexpected device %+v", device.GetInfo())
	}
	if got := len(m.states()); got != 1 {
		t.Errorf("only the reachable appliance may be announced, got %v", m.states())
	}

	// The live appliance polls while the dead one is still retrying.
	if _, err := c.read(samsung.ResDoors, mapDoors)(device, nil); err != nil {
		t.Errorf("the reachable appliance must be readable, got %v", err)
	}
}

// TestSecondApplianceWithSameDeviceId: one refrigerator reachable under two
// configured addresses must not be announced twice. Two devices with one id
// would overwrite each other's state and events in the device-manager.
func TestSecondApplianceWithSameDeviceId(t *testing.T) {
	first := newFakeOCF()
	second := newFakeOCF() // same di
	c := testConnector(first, second)
	m := &fakeMgw{}

	c.Start(t.Context(), m)
	waitFor(t, func() bool { _, ok := m.last(); return ok }, "no device announced")
	time.Sleep(20 * time.Millisecond)

	if got := len(m.states()); got != 1 {
		t.Errorf("the duplicate appliance must be dropped, got %v", m.states())
	}
}

// TestTwoAppliancesAreServedIndependently: each appliance is its own device,
// reads and writes go to its own transport, and one going offline leaves the
// other polling.
func TestTwoAppliancesAreServedIndependently(t *testing.T) {
	kitchen := newFakeOCF()
	kitchen.representations[ocfDeviceResource] = map[string]any{ocfDeviceIdKey: "kitchen-di"}
	cellar := newFakeOCF()
	cellar.representations[ocfDeviceResource] = map[string]any{ocfDeviceIdKey: "cellar-di"}
	c := testConnector(kitchen, cellar)
	m := &fakeMgw{}

	c.Start(t.Context(), m)
	waitFor(t, func() bool { return len(m.states()) == 2 },
		"both appliances must be announced, got %v", m.states)

	kitchenDev := Device{Id: "kitchen-di", DeviceTypeId: testDeviceTypeId, Online: true}
	cellarDev := Device{Id: "cellar-di", DeviceTypeId: testDeviceTypeId, Online: true}

	// A command goes to the transport of the device it was addressed to.
	if _, err := c.write(samsung.SvcSetRapidFridge)(cellarDev, true); err != nil {
		t.Fatal(err)
	}
	if got := len(cellar.sentPosts()); got != 1 {
		t.Errorf("the addressed appliance must get the write, got %d posts", got)
	}
	if got := len(kitchen.sentPosts()); got != 0 {
		t.Errorf("the other appliance must not be written to, got %d posts", got)
	}

	// An unknown device id is reported, not written to one of them at random.
	if _, err := c.write(samsung.SvcSetRapidFridge)(Device{Id: "no-such-di"}, true); err == nil {
		t.Error("a command for an unknown device id must be rejected")
	}

	// One appliance failing takes only itself offline.
	kitchen.setDown(true)
	read := c.read(samsung.ResDoors, mapDoors)
	for i := 0; i < offlineAfterFailures; i++ {
		if _, err := read(kitchenDev, nil); err == nil {
			t.Fatal("read must report the transport error")
		}
	}
	waitFor(t, func() bool {
		for _, d := range m.snapshot() {
			if d.Id == "kitchen-di" && !d.Online {
				return true
			}
		}
		return false
	}, "the failing appliance must be announced offline, got %v", m.states)

	for _, d := range m.snapshot() {
		if d.Id == "cellar-di" && !d.Online {
			t.Error("the other appliance must stay online")
		}
	}
	if _, err := read(cellarDev, nil); err != nil {
		t.Errorf("the other appliance must stay readable, got %v", err)
	}
}

// TestOnlineOfflineOnline covers defect 1: failing reads take the device offline
// once, and the probe brings it back when the appliance returns.
func TestOnlineOfflineOnline(t *testing.T) {
	ocf := newFakeOCF()
	c, m, dev := startedConnector(t, ocf)

	read := c.read(samsung.ResDoors, mapDoors)
	ocf.setDown(true)
	for i := 0; i < offlineAfterFailures-1; i++ {
		if _, err := read(dev, nil); err == nil {
			t.Fatal("read must report the transport error")
		}
		if got := m.states(); len(got) != 1 {
			t.Fatalf("below the threshold the state must not change, got %v after %d failures", got, i+1)
		}
	}
	if _, err := read(dev, nil); err == nil {
		t.Fatal("read must report the transport error")
	}
	if got := m.states(); !reflect.DeepEqual(got, []string{"online", "offline"}) {
		t.Fatalf("device must be announced offline at the threshold, got %v", got)
	}

	// further failed reads must not repeat the offline update
	if _, err := read(dev, nil); err == nil {
		t.Fatal("read must report the transport error")
	}
	if got := m.states(); len(got) != 2 {
		t.Errorf("offline must be announced once, got %v", got)
	}

	// the library stops polling an offline device, so only the probe Announce
	// started can notice the appliance's return
	ocf.setDown(false)
	waitFor(t, func() bool {
		return reflect.DeepEqual(m.states(), []string{"online", "offline", "online"})
	}, "device must come back online, got %v", m.states)

	// back online the read works again and does not re-announce
	if _, err := read(dev, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(m.states()); got != 3 {
		t.Errorf("no further device update expected, got %d", got)
	}
}

// TestFailedOfflineAnnounceIsRetried: a device update lost on the broker must
// not leave the connector believing the device is offline.
func TestFailedOfflineAnnounceIsRetried(t *testing.T) {
	ocf := newFakeOCF()
	c, m, dev := startedConnector(t, ocf)

	m.mux.Lock()
	m.err = errors.New("mqtt client not connected")
	m.failCount = 1
	m.mux.Unlock()

	ocf.setDown(true)
	read := c.read(samsung.ResEnergy, mapEnergy)
	for i := 0; i < offlineAfterFailures; i++ {
		_, _ = read(dev, nil)
	}
	if got := m.states(); !reflect.DeepEqual(got, []string{"online"}) {
		t.Fatalf("the failed update must not count as announced, got %v", got)
	}
	_, _ = read(dev, nil)
	if got := m.states(); !reflect.DeepEqual(got, []string{"online", "offline"}) {
		t.Errorf("the next failed read must retry the offline update, got %v", got)
	}
}

// TestRefresh: before the announce there is nothing to re-announce; afterwards
// the current state of every known appliance goes out again.
func TestRefresh(t *testing.T) {
	bare := testConnector(newFakeOCF())
	bare.Refresh()

	ocf := newFakeOCF()
	c, m, dev := startedConnector(t, ocf)

	ocf.setDown(true)
	read := c.read(samsung.ResDoors, mapDoors)
	for i := 0; i < offlineAfterFailures; i++ {
		_, _ = read(dev, nil)
	}

	c.Refresh()
	if got := m.states(); !reflect.DeepEqual(got, []string{"online", "offline", "offline"}) {
		t.Errorf("refresh must re-announce the current state, got %v", got)
	}
}

// TestReadMapsRepresentation: the read handler returns the mapped event, and a
// transport error reaches the library unchanged.
func TestReadMapsRepresentation(t *testing.T) {
	ocf := newFakeOCF()
	c, _, dev := startedConnector(t, ocf)
	res, err := c.read(samsung.ResDoors, mapDoors)(dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	event, ok := res.(samsung.DoorEvent)
	if !ok {
		t.Fatalf("expected a DoorEvent, got %T", res)
	}
	if event.FridgeOpen == nil || !*event.FridgeOpen || event.AnyOpen == nil || !*event.AnyOpen {
		t.Errorf("open fridge door not mapped: %+v", event)
	}
	if event.FreezerOpen != nil {
		t.Errorf("an unread freezer door must stay nil, got %v", *event.FreezerOpen)
	}

	ocf.setDown(true)
	res, err = c.read(samsung.ResDoors, mapDoors)(dev, nil)
	if !errors.Is(err, errUnreachable) || res != nil {
		t.Errorf("transport error must be passed on, got %v / %v", res, err)
	}
}

// TestWrite covers the command path: a mapped command is posted, an unknown
// service id and a failing write are reported without a post slipping through.
func TestWrite(t *testing.T) {
	ocf := newFakeOCF()
	c, _, dev := startedConnector(t, ocf)

	if _, err := c.write(samsung.SvcSetRapidFridge)(dev, true); err != nil {
		t.Fatal(err)
	}
	posts := ocf.sentPosts()
	if len(posts) != 1 || posts[0].path != samsung.ResRefrigeration {
		t.Fatalf("unexpected write %+v", posts)
	}
	if posts[0].body[samsung.KeyRapidFridge] != samsung.ValueOn {
		t.Errorf("unexpected body %+v", posts[0].body)
	}

	if _, err := c.write("no_such_service")(dev, true); err == nil {
		t.Error("an unknown service id must be rejected")
	}
	if _, err := c.write(samsung.SvcSetFridgeSetpoint)(dev, "not a number"); err == nil {
		t.Error("an unusable input must be rejected")
	}
	if got := len(ocf.sentPosts()); got != 1 {
		t.Errorf("a rejected command must not be written, got %d posts", got)
	}

	ocf.mux.Lock()
	ocf.postErr = errors.New("post failed")
	ocf.mux.Unlock()
	if _, err := c.write(samsung.SvcSetRapidFreezing)(dev, false); err == nil {
		t.Error("a failed write must be reported")
	}
}

// TestConcurrentReads exercises the shared reachability state the way the
// library drives it: all read services run in parallel on every poll round.
func TestConcurrentReads(t *testing.T) {
	ocf := newFakeOCF()
	for _, path := range []string{samsung.ResEnergy, samsung.ResRefrigeration} {
		ocf.representations[path] = map[string]any{}
	}
	c, m, dev := startedConnector(t, ocf)
	ocf.setDown(true)

	reads := []mgw.ServiceFunc[Device]{
		c.read(samsung.ResTemperatures, mapTemperatures),
		c.read(samsung.ResDoors, mapDoors),
		c.read(samsung.ResEnergy, mapEnergy),
		c.read(samsung.ResRefrigeration, mapRefrigeration),
	}
	for round := 0; round < 4; round++ {
		wg := sync.WaitGroup{}
		for _, read := range reads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = read(dev, nil)
			}()
		}
		wg.Wait()
		go c.Refresh()
	}

	waitFor(t, func() bool {
		for _, state := range m.states() {
			if state == "offline" {
				return true
			}
		}
		return false
	}, "device must be announced offline, got %v", m.states)
}

// TestSetpointUsesLastTemperatureRead: the setpoint write addresses the item the
// temperature read reported, by its id, and is checked against the range of that
// same item.
func TestSetpointUsesLastTemperatureRead(t *testing.T) {
	ocf := newFakeOCF()
	c, _, dev := startedConnector(t, ocf)
	if _, err := c.read(samsung.ResTemperatures, mapTemperatures)(dev, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := c.write(samsung.SvcSetFridgeSetpoint)(dev, 5); err != nil {
		t.Fatal(err)
	}
	posts := ocf.sentPosts()
	if len(posts) != 1 || posts[0].path != samsung.ResTemperatures {
		t.Fatalf("unexpected write %+v", posts)
	}
	items, ok := posts[0].body[samsung.KeyItems].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected body %+v", posts[0].body)
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected item %+v", items[0])
	}
	if item[samsung.KeyID] != uint64(0) {
		t.Errorf("the write must carry the id the read reported, got %#v", item[samsung.KeyID])
	}
	if item[samsung.KeyDescription] != "Fridge" {
		t.Errorf("the write must carry the description the device reported, got %#v", item[samsung.KeyDescription])
	}
	if item[samsung.KeyDesired] != 5.0 {
		t.Errorf("unexpected setpoint %#v", item[samsung.KeyDesired])
	}
}

// TestSetpointOutOfRangeIsRejected: the range the device reported is the one
// that counts, and a value outside it never reaches the appliance.
func TestSetpointOutOfRangeIsRejected(t *testing.T) {
	ocf := newFakeOCF()
	c, _, dev := startedConnector(t, ocf)
	if _, err := c.read(samsung.ResTemperatures, mapTemperatures)(dev, nil); err != nil {
		t.Fatal(err)
	}

	for _, value := range []any{999, -40} {
		if _, err := c.write(samsung.SvcSetFridgeSetpoint)(dev, value); err == nil {
			t.Errorf("setpoint %v is outside the reported range and must be rejected", value)
		}
	}
	if got := len(ocf.sentPosts()); got != 0 {
		t.Errorf("a rejected setpoint must not be written, got %d posts", got)
	}
	// the bound itself is still allowed
	if _, err := c.write(samsung.SvcSetFridgeSetpoint)(dev, 7); err != nil {
		t.Errorf("the reported maximum must be writable, got %v", err)
	}
}

// TestSetpointWithoutTemperatureReadIsRejected: before the first temperature
// read there is no range to check against, so the setpoint is refused. The
// rapid-mode commands address no compartment and stay unaffected.
func TestSetpointWithoutTemperatureReadIsRejected(t *testing.T) {
	ocf := newFakeOCF()
	c, _, dev := startedConnector(t, ocf)

	for _, service := range []string{samsung.SvcSetFridgeSetpoint, samsung.SvcSetFreezerSetpoint} {
		if _, err := c.write(service)(dev, 5); err == nil {
			t.Errorf("%s must be rejected without a temperature reading", service)
		}
	}
	if got := len(ocf.sentPosts()); got != 0 {
		t.Errorf("nothing may be written, got %d posts", got)
	}
	if _, err := c.write(samsung.SvcSetRapidFridge)(dev, true); err != nil {
		t.Errorf("a rapid-mode command needs no temperature reading, got %v", err)
	}

	// a failed temperature read leaves the connector without a representation
	ocf.setDown(true)
	if _, err := c.read(samsung.ResTemperatures, mapTemperatures)(dev, nil); err == nil {
		t.Fatal("expected the transport error")
	}
	if _, err := c.write(samsung.SvcSetFridgeSetpoint)(dev, 5); err == nil {
		t.Error("a failed read must not enable the setpoint write")
	}
}

// TestSetpointDuringPolling runs the command path against the running read
// services; -race proves the representation is handed over under the lock.
func TestSetpointDuringPolling(t *testing.T) {
	ocf := newFakeOCF()
	ocf.representations[samsung.ResEnergy] = map[string]any{}
	c, _, dev := startedConnector(t, ocf)
	if _, err := c.read(samsung.ResTemperatures, mapTemperatures)(dev, nil); err != nil {
		t.Fatal(err)
	}

	reads := []mgw.ServiceFunc[Device]{
		c.read(samsung.ResTemperatures, mapTemperatures),
		c.read(samsung.ResDoors, mapDoors),
		c.read(samsung.ResEnergy, mapEnergy),
	}
	wg := sync.WaitGroup{}
	for i := 0; i < 20; i++ {
		for _, read := range reads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = read(dev, nil)
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.write(samsung.SvcSetFridgeSetpoint)(dev, 4); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := len(ocf.sentPosts()); got != 20 {
		t.Errorf("expected every setpoint to be written, got %d", got)
	}
}

// TestAnnounceDoesNotBlockReads: the device update travels over MQTT, and the
// library waits for the broker without a deadline of its own. An appliance must
// therefore stay readable while one of its updates is stuck, or a silent broker
// would stall this appliance's reads and, through the library's per-service
// wait, the next poll round of every other appliance with it.
func TestAnnounceDoesNotBlockReads(t *testing.T) {
	ocf := newFakeOCF()
	c, m, dev := startedConnector(t, ocf)

	hold := make(chan struct{})
	m.mux.Lock()
	m.hold = hold
	m.mux.Unlock()

	// Drive the appliance offline. The read that crosses the threshold hands
	// the update to the broker, which does not answer.
	ocf.setDown(true)
	read := c.read(samsung.ResDoors, mapDoors)
	go func() {
		for i := 0; i < offlineAfterFailures; i++ {
			_, _ = read(dev, nil)
		}
	}()
	waitFor(t, func() bool { return m.blockedAnnounces() == 1 },
		"the offline update must reach the broker and stall there")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = read(dev, nil)
		_, _ = c.write(samsung.SvcSetRapidFridge)(dev, true)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(hold)
		t.Fatal("a stalled device update must not block the appliance's reads and commands")
	}
	close(hold)
}

// waitFor polls condition for up to two seconds; the probe and Refresh run in
// their own goroutines, so there is nothing to synchronise on.
func waitFor(t *testing.T, condition func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	values := make([]any, 0, len(args))
	for _, arg := range args {
		if f, ok := arg.(func() []string); ok {
			values = append(values, f())
			continue
		}
		values = append(values, arg)
	}
	t.Fatalf(format, values...)
}

func TestReadServicesCoverContract(t *testing.T) {
	registered := make(map[string]bool, len(readServices))
	for _, r := range readServices {
		if registered[r.localId] {
			t.Errorf("read service %q is registered twice", r.localId)
		}
		registered[r.localId] = true
	}
	for _, svc := range samsung.ReadServices {
		if !registered[svc] {
			t.Errorf("read service %q is in samsung.ReadServices but not registered", svc)
		}
		delete(registered, svc)
	}
	for svc := range registered {
		t.Errorf("read service %q is registered but missing from samsung.ReadServices", svc)
	}
}

func TestProbeIntervalFor(t *testing.T) {
	cases := []struct {
		name      string
		intervals map[string]time.Duration
		want      time.Duration
	}{
		{
			name:      "empty falls back to the default",
			intervals: map[string]time.Duration{},
			want:      defaultProbeInterval,
		},
		{
			name:      "only non-positive values fall back to the default",
			intervals: map[string]time.Duration{samsung.SvcDoors: 0},
			want:      defaultProbeInterval,
		},
		{
			name: "the fastest service sets the cadence",
			intervals: map[string]time.Duration{
				samsung.SvcDoors:         45 * time.Second,
				samsung.SvcTemperatures:  2 * time.Minute,
				samsung.SvcRefrigeration: 5 * time.Minute,
			},
			want: 45 * time.Second,
		},
		{
			name: "a fast service is floored at minProbeInterval",
			intervals: map[string]time.Duration{
				samsung.SvcDoors:        5 * time.Second,
				samsung.SvcTemperatures: time.Minute,
			},
			want: minProbeInterval,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeIntervalFor(tc.intervals); got != tc.want {
				t.Errorf("probeIntervalFor = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestIcemakerCommandUsesLastRead(t *testing.T) {
	ocf := newFakeOCF()
	c, _, dev := startedConnector(t, ocf)

	// Before the first read the connector does not know the control scheme.
	if _, err := c.write(samsung.SvcSetIcemaker)(dev, false); err == nil {
		t.Error("the ice maker command must be rejected without a reading")
	}
	if got := len(ocf.sentPosts()); got != 0 {
		t.Fatalf("nothing may be written before the first read, got %d posts", got)
	}

	if _, err := c.read(samsung.ResIcemaker, mapIcemaker)(dev, nil); err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if _, err := c.write(samsung.SvcSetIcemaker)(dev, false); err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	posts := ocf.sentPosts()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(posts))
	}
	if posts[0].path != samsung.ResIcemaker {
		t.Errorf("post path = %q, want %q", posts[0].path, samsung.ResIcemaker)
	}
	if got := posts[0].body[samsung.KeyIceMakerState]; got != samsung.ValueOff {
		t.Errorf("wrote %v, want %q", got, samsung.ValueOff)
	}
}

func TestIcemakerCommandRejectsUnknownControlScheme(t *testing.T) {
	ocf := newFakeOCF()
	ocf.representations[samsung.ResIcemaker][samsung.KeyIceMakerType] = "slider"
	c, _, dev := startedConnector(t, ocf)

	if _, err := c.read(samsung.ResIcemaker, mapIcemaker)(dev, nil); err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if _, err := c.write(samsung.SvcSetIcemaker)(dev, true); err == nil {
		t.Error("an ice maker whose write form is unknown must not be written")
	}
	if got := len(ocf.sentPosts()); got != 0 {
		t.Errorf("nothing may be written, got %d posts", got)
	}
}

func TestCommandResourcesCoverSetServices(t *testing.T) {
	for _, svc := range setServices {
		path := mapping.ReadResourceFor(svc)
		if path == "" {
			continue
		}
		if !commandResources[path] {
			t.Errorf("set service %q is built against %s, but that resource is not kept", svc, path)
		}
	}
	for path := range commandResources {
		var polled bool
		for _, r := range readServices {
			if r.path == path {
				polled = true
			}
		}
		if !polled {
			t.Errorf("resource %s is kept for a command but never read by a polled service", path)
		}
	}
}
