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

// Shared contract for the mgw-samsung-dc device connector.
//
// This file is the single source of truth that every other file in the package
// builds against: the OCF transport implements OCFClient, the event mapping and
// the command mapping are written against the resource paths, keys and service
// ids declared here. Change a name here, not in a consumer.
//
// The device speaks OCF over CoAP/DTLS. Resource representations are CBOR maps;
// the transport decodes them to map[string]any, and the mapping extracts values
// by the x.com.samsung.da.* keys below. Field keys are taken from a proven
// Samsung refrigerator resource schema; the exact per-item id-to-compartment
// assignment is confirmed against the live device during verification, so the
// mapping keys compartments by the item's description, never by a hardcoded id.

package samsung

import "context"

// OCFClient is the transport to one appliance. The OCF/CoAP/DTLS implementation
// in pkg/samsung/ocf satisfies it; the connector wiring and command mapping
// depend only on this interface, never on the transport package.
type OCFClient interface {
	// Get reads one OCF resource and returns its CBOR representation decoded to
	// a generic map. path is an absolute OCF resource path, e.g. ResTemperatures.
	Get(ctx context.Context, path string) (map[string]any, error)
	// Post writes a CBOR-encoded body to one OCF resource (OCF UPDATE).
	Post(ctx context.Context, path string, body map[string]any) error
	// Close releases the DTLS session.
	Close() error
}

// OCF resource paths on the appliance.
const (
	ResTemperatures  = "/temperatures/vs/0"       // items[]: current/desired/min/max temps per compartment
	ResDoors         = "/doors/vs/0"              // items[]: open/close state per door
	ResEnergy        = "/energy/consumption/vs/0" // instantaneous power + cumulative consumption
	ResRefrigeration = "/refrigeration/vs/0"      // rapidFridge / rapidFreezing booleans-as-On/Off
	ResIcemaker      = "/icemaker/one/vs/0"       // ice maker state + status
	ResDeviceInfo    = "/information/vs/0"        // serial number etc. (device announce)
)

// CBOR representation keys inside the resources above. All Samsung vendor keys
// carry the x.com.samsung.da. prefix.
const (
	KeyItems       = "x.com.samsung.da.items"       // array field in /temperatures and /doors
	KeyDescription = "x.com.samsung.da.description" // compartment label inside a temperature item ("fridge"/"freezer"/...)
	KeyID          = "x.com.samsung.da.id"          // item id inside a temperature/door item
	KeyCurrent     = "x.com.samsung.da.current"     // current temperature
	KeyDesired     = "x.com.samsung.da.desired"     // setpoint temperature (also the write target)
	KeyMinimum     = "x.com.samsung.da.minimum"
	KeyMaximum     = "x.com.samsung.da.maximum"
	KeyOpenState   = "x.com.samsung.da.openState"          // "Open" / "Close"
	KeyInstPower   = "x.com.samsung.da.instantaneousPower" // watts
	// KeyCumulativeEnergy is the cumulative energy counter, despite the name the
	// appliance gives it: the resource reports its unit in
	// x.com.samsung.da.cumulativeUnit ("Wh"), and the value rises. Two reads
	// 39m56s apart on a TP1X_REF_21K gave 6627 and 6647, so +20 Wh, a 30 W
	// average between an instantaneous 4 W and 48 W in the same two reads.
	KeyCumulativeEnergy = "x.com.samsung.da.cumulativePower"
	// KeyCumulativeConsumption is NOT the energy counter, despite the name: the
	// same two reads gave 521 and 21, a drop of 500 between them. What it counts
	// and when it resets is unknown, so it is not emitted. A counter that falls
	// makes every difference a consumer builds from it negative, which reads as
	// energy flowing backwards out of a fridge.
	KeyCumulativeConsumption = "x.com.samsung.da.cumulativeConsumption"
	KeyRapidFridge           = "x.com.samsung.da.rapidFridge"   // "On" / "Off"
	KeyRapidFreezing         = "x.com.samsung.da.rapidFreezing" // "On" / "Off"

	// Ice maker keys. The appliance reports them as flat dotted keys inside
	// ResIcemaker, not as a nested object:
	//
	//	{"x.com.samsung.da.iceMaker.state":"On",
	//	 "x.com.samsung.da.iceMaker.iceMakingStatus":"ICESTATUS_STOP",
	//	 "x.com.samsung.da.iceMaker.type":"toggle",
	//	 "x.com.samsung.da.iceMaker.name":"ICE_MAKER",
	//	 "x.com.samsung.da.iceType.desired":"NORMAL"}
	//
	// confirmed on a TP1X_REF_21K.
	KeyIceMakerState  = "x.com.samsung.da.iceMaker.state"           // "On" / "Off", the switchable part
	KeyIceMakerStatus = "x.com.samsung.da.iceMaker.iceMakingStatus" // what the ice maker is doing right now
	KeyIceMakerType   = "x.com.samsung.da.iceMaker.type"            // "toggle" on the confirmed model
	KeyIceTypeDesired = "x.com.samsung.da.iceType.desired"          // "NORMAL"; read only, see below
)

// IceMakerTypeToggle is the only ice maker type the command mapping writes to.
// The appliance reports how its ice maker is operated; only "toggle" is known to
// take an On/Off write on KeyIceMakerState. A representation reporting another
// type is rejected instead of written blind, because an unknown control scheme
// is exactly what must not reach an appliance.
const IceMakerTypeToggle = "toggle"

// String enum values used in the representations above.
const (
	StateOpen  = "Open"
	StateClose = "Close"
	ValueOn    = "On"
	ValueOff   = "Off"
)

// Compartment descriptions as they appear in KeyDescription. The live device may
// report a different casing or set; the mapping matches case-insensitively and
// tolerates unknown compartments rather than failing.
const (
	CompartmentFridge  = "fridge"
	CompartmentFreezer = "freezer"
)

// Service local ids. These are the RegisterService local ids and the trailing
// segment of the command topic command/<deviceId>/<serviceLocalId>. Read
// services carry a poll duration; set services are invoked per command.
const (
	// Read services (polled).
	SvcTemperatures  = "temperatures"  // emits fridge/freezer current + setpoint
	SvcDoors         = "doors"         // emits per-door open state
	SvcEnergy        = "energy"        // emits instantaneous power + cumulative Wh
	SvcRefrigeration = "refrigeration" // emits rapidFridge / rapidFreezing
	SvcIcemaker      = "icemaker"      // emits ice maker on/off + what it is doing

	// Set services (command-driven).
	SvcSetFridgeSetpoint  = "set_fridge_setpoint"  // input: temperature value -> ResTemperatures desired
	SvcSetFreezerSetpoint = "set_freezer_setpoint" // input: temperature value -> ResTemperatures desired
	SvcSetRapidFridge     = "set_rapid_fridge"     // input: On/Off -> ResRefrigeration rapidFridge
	SvcSetRapidFreezing   = "set_rapid_freezing"   // input: On/Off -> ResRefrigeration rapidFreezing
	SvcSetIcemaker        = "set_icemaker"         // input: On/Off -> ResIcemaker iceMaker.state
)

// ReadServices are the polled read services, in registration order. It is the
// list the configuration derives the per-service POLL_INTERVAL_<SERVICE>
// variables from and the list the connector registers, so a read service added
// here becomes configurable and registered in one step.
var ReadServices = []string{
	SvcTemperatures,
	SvcDoors,
	SvcEnergy,
	SvcRefrigeration,
	SvcIcemaker,
}

// Reading is the normalized snapshot the event mapping produces from the OCF
// resource maps. The connector wiring turns one Reading field group into one
// MarshalAndSendEvent payload per read service. Pointer fields are nil when the
// device did not report that value, so a partial read never emits a zero.
//
// Consumer signatures (pinned here so the wiring and the mapping agree):
//
//	// implemented in mapping.go:
//	func MapTemperatures(res map[string]any) TemperatureEvent
//	func MapDoors(res map[string]any) DoorEvent
//	func MapEnergy(res map[string]any) EnergyEvent
//	func MapRefrigeration(res map[string]any) RefrigerationEvent
//	func MapIcemaker(res map[string]any) IcemakerEvent
//
//	// implemented in command.go — returns the OCF resource path and CBOR body
//	// for a SENERGY command input, or an error for an unknown/invalid input:
//	func BuildCommand(serviceLocalId string, input any) (path string, body map[string]any, err error)
//
//	// same, but addresses the compartment by the id of the item read last and
//	// checks the value against that item's minimum and maximum. The wiring keeps
//	// the last /temperatures representation and calls this one; BuildCommand
//	// falls back to the compartment description and checks no range:
//	func BuildCommandFromTemperatures(serviceLocalId string, input any, temperatures map[string]any) (path string, body map[string]any, err error)
type TemperatureEvent struct {
	FridgeCurrent   *float64 `json:"fridge_temp_c,omitempty"`
	FridgeSetpoint  *float64 `json:"fridge_setpoint_c,omitempty"`
	FreezerCurrent  *float64 `json:"freezer_temp_c,omitempty"`
	FreezerSetpoint *float64 `json:"freezer_setpoint_c,omitempty"`
}

// DoorEvent carries the door states. The pointer fields follow the same rule as
// the temperatures: nil means the device did not report that door, not "closed".
// A compartment flag is set only when at least one door of that compartment was
// read, AnyOpen only when at least one door at all was read — otherwise a
// renamed or missing CBOR key would be reported as "everything closed" instead
// of as a missing value.
type DoorEvent struct {
	FridgeOpen  *bool `json:"fridge_door_open,omitempty"`
	FreezerOpen *bool `json:"freezer_door_open,omitempty"`
	AnyOpen     *bool `json:"any_door_open,omitempty"`
}

type EnergyEvent struct {
	PowerW       *float64 `json:"power_w,omitempty"`
	CumulativeWh *float64 `json:"energy_wh_cumulative,omitempty"`
}

// RefrigerationEvent carries the rapid-cooling flags. A field is nil when the
// resource did not carry its key, so an unexpected key name reads as "unknown"
// instead of as a reported "off".
type RefrigerationEvent struct {
	RapidFridge   *bool `json:"rapid_fridge,omitempty"`
	RapidFreezing *bool `json:"rapid_freezing,omitempty"`
}

// IcemakerEvent carries the ice maker. On is the switch the set service writes.
//
// Status is passed through as the appliance spells it, e.g. "ICESTATUS_STOP".
// Only that one value has been observed, so the full set is unknown and no
// boolean "is making ice" is derived from it: a derivation would have to assume
// which of the unseen values mean running, and a wrong assumption reads as a
// fact in the UI. A consumer can match the string once the other values are
// known.
//
// KeyIceTypeDesired ("NORMAL") is not carried: whether it is writable, and which
// other ice types the appliance accepts, is unknown.
type IcemakerEvent struct {
	On     *bool   `json:"ice_maker_on,omitempty"`
	Status *string `json:"ice_making_status,omitempty"`
}
