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

package mapping

import (
	"fmt"
	"strings"

	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung"
)

// BuildCommand translates a SENERGY command, addressed by its service local id,
// into the OCF resource path and CBOR body that writes it to the appliance. It
// returns an error for an unknown service id or an input the service cannot use.
//
// It builds the command without the representation the service writes into,
// which every service that needs one rejects: see
// BuildCommandFromRepresentation. It therefore only ever succeeds for the rapid
// modes, which address no compartment.
func BuildCommand(serviceLocalId string, input any) (path string, body map[string]any, err error) {
	return BuildCommandFromRepresentation(serviceLocalId, input, nil)
}

// BuildCommandFromRepresentation is BuildCommand with the last read
// representation of the resource the command writes into: /temperatures for a
// setpoint, ResIcemaker for the ice maker, nothing for the rapid modes.
// ReadResourceFor names the resource a service needs.
//
// A setpoint uses it to address the item by the id and description the device
// itself reported for that compartment, and to reject a value outside the
// KeyMinimum/KeyMaximum range the same item reported. A representation that is
// nil, empty or carries no item for the compartment rejects the command; see
// buildSetpoint for why there is no fallback.
//
// The ice maker uses it to check that the appliance reports a control scheme
// this package knows how to write, and rejects the command otherwise.
func BuildCommandFromRepresentation(serviceLocalId string, input any, representation map[string]any) (path string, body map[string]any, err error) {
	temperatures := representation
	icemaker := representation
	switch serviceLocalId {
	case samsung.SvcSetRapidFridge:
		v, err := toOnOff(input)
		if err != nil {
			return "", nil, err
		}
		return samsung.ResRefrigeration, map[string]any{samsung.KeyRapidFridge: v}, nil
	case samsung.SvcSetRapidFreezing:
		v, err := toOnOff(input)
		if err != nil {
			return "", nil, err
		}
		return samsung.ResRefrigeration, map[string]any{samsung.KeyRapidFreezing: v}, nil
	case samsung.SvcSetIcemaker:
		return buildIcemaker(input, icemaker)
	case samsung.SvcSetFridgeSetpoint:
		return buildSetpoint(samsung.CompartmentFridge, input, temperatures)
	case samsung.SvcSetFreezerSetpoint:
		return buildSetpoint(samsung.CompartmentFreezer, input, temperatures)
	default:
		return "", nil, fmt.Errorf("mapping: unknown service local id %q", serviceLocalId)
	}
}

// ReadResourceFor names the OCF resource whose last read representation a set
// service is built against, or "" for a service that needs none. The connector
// uses it to keep exactly the representations the commands need.
func ReadResourceFor(serviceLocalId string) string {
	switch serviceLocalId {
	case samsung.SvcSetFridgeSetpoint, samsung.SvcSetFreezerSetpoint:
		return samsung.ResTemperatures
	case samsung.SvcSetIcemaker:
		return samsung.ResIcemaker
	default:
		return ""
	}
}

// buildIcemaker builds the ice maker write.
//
// NAMED ASSUMPTION: the ice maker is switched by writing the On/Off string to
// KeyIceMakerState on ResIcemaker, in the same flat shape the read returns.
// Unlike the rapid modes, this write has not been confirmed against a live
// appliance; the read has.
//
// The write is refused without a representation, and refused when the
// representation reports a type other than IceMakerTypeToggle. The appliance
// states how its ice maker is operated, and a model that is not a toggle is one
// whose write form is unknown — writing On into it anyway is a guess at an
// appliance, which is what the setpoint path already refuses to do.
func buildIcemaker(input any, icemaker map[string]any) (string, map[string]any, error) {
	v, err := toOnOff(input)
	if err != nil {
		return "", nil, err
	}
	if icemaker == nil {
		return "", nil, fmt.Errorf("mapping: no ice maker reading yet, the control scheme of the appliance is unknown")
	}
	if t := asString(icemaker[samsung.KeyIceMakerType]); t != samsung.IceMakerTypeToggle {
		return "", nil, fmt.Errorf("mapping: ice maker type %q is not %q, the write form is unknown", t, samsung.IceMakerTypeToggle)
	}
	return samsung.ResIcemaker, map[string]any{samsung.KeyIceMakerState: v}, nil
}

// buildSetpoint builds the /temperatures write for one compartment's setpoint.
//
// NAMED ASSUMPTION: a setpoint is written as a single items entry that
// identifies the compartment and carries the new value under the desired key:
//
//	{ KeyItems: [ { KeyID: <id>, KeyDescription: <desc>, KeyDesired: <value> } ] }
//
// The exact write form — a single-item items array, the value under KeyDesired —
// is confirmed against the live device during verification (see contract.go). It
// is kept in one place here so the live check has a single thing to confirm.
//
// Id and description are taken from the item the device reported for this
// compartment in temperatures, so the write addresses what the read addressed.
//
// Without that item the command is refused. There is no fallback to the
// compartment constant: the read path holds the device's spelling of a
// compartment to be unknown, so the fallback would address a guessed
// description, and checkRange has no minimum and maximum to check the value
// against either. That combination — an unverified value sent to a guessed
// address — is the one thing a setpoint write must never be, and it is reached
// by an empty representation as easily as by a missing one: an OCF read of a
// resource with an empty body is a success returning an empty map, not an
// error. The ice maker refuses the same way for the same reason.
func buildSetpoint(compartment string, input any, temperatures map[string]any) (string, map[string]any, error) {
	n, err := toNumber(input)
	if err != nil {
		return "", nil, err
	}
	item := compartmentItem(compartment, temperatures)
	if item == nil {
		return "", nil, fmt.Errorf(
			"mapping: no reading of the %s compartment yet, its setpoint range and the way the appliance addresses it are unknown",
			compartment)
	}
	if err := checkRange(compartment, n, item); err != nil {
		return "", nil, err
	}
	target := map[string]any{samsung.KeyDesired: n}
	if id, ok := item[samsung.KeyID]; ok {
		target[samsung.KeyID] = id
	}
	if desc, ok := item[samsung.KeyDescription]; ok {
		target[samsung.KeyDescription] = desc
	}
	body := map[string]any{samsung.KeyItems: []any{target}}
	return samsung.ResTemperatures, body, nil
}

// compartmentItem picks the item the device reported for one compartment out of
// a /temperatures representation, by the same exact description match the event
// mapping uses, so a sub-zone is never taken for the compartment. It returns nil
// when there is no representation or no matching item.
func compartmentItem(compartment string, temperatures map[string]any) map[string]any {
	for _, item := range items(temperatures) {
		if exactCompartmentOf(item) == compartment {
			return item
		}
	}
	return nil
}

// checkRange rejects a setpoint outside the range the device reported for that
// compartment. A missing bound is not checked: the device decides the range, and
// this package does not invent one.
func checkRange(compartment string, value float64, item map[string]any) error {
	if min := toFloat(item[samsung.KeyMinimum]); min != nil && value < *min {
		return fmt.Errorf("mapping: %s setpoint %v below the device minimum %v", compartment, value, *min)
	}
	if max := toFloat(item[samsung.KeyMaximum]); max != nil && value > *max {
		return fmt.Errorf("mapping: %s setpoint %v above the device maximum %v", compartment, value, *max)
	}
	return nil
}

// toOnOff maps a rapid-mode command input to the canonical On/Off string the
// device expects. It accepts a bool or an "On"/"Off" string (case-insensitive,
// surrounding space tolerated); anything else is an error.
func toOnOff(input any) (string, error) {
	switch v := input.(type) {
	case bool:
		if v {
			return samsung.ValueOn, nil
		}
		return samsung.ValueOff, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "on":
			return samsung.ValueOn, nil
		case "off":
			return samsung.ValueOff, nil
		}
	}
	return "", fmt.Errorf("mapping: invalid on/off input %v (%T)", input, input)
}

// toNumber pulls a setpoint value out of a command input. It accepts the same
// numeric forms as the event mapping (int64, uint64, float64, numeric strings);
// a missing or non-numeric input is an error.
func toNumber(input any) (float64, error) {
	if f := toFloat(input); f != nil {
		return *f, nil
	}
	return 0, fmt.Errorf("mapping: invalid numeric input %v (%T)", input, input)
}
