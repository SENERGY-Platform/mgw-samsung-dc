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

// Package mapping translates between the generic OCF resource maps the Samsung
// transport decodes from CBOR and the normalized SENERGY event structs declared
// in the samsung contract. It is pure: no network, no transport, stdlib only.
//
// All resource paths, representation keys, service ids and enum values come from
// the contract in the parent package; this package imports them and never
// redeclares a name.
package mapping

import (
	"strconv"
	"strings"

	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung"
)

// MapTemperatures reads the /temperatures items into a TemperatureEvent. An
// item is assigned to a compartment only when its description is exactly that
// compartment's name (case-insensitive, surrounding space tolerated); current
// and setpoint come from KeyCurrent and KeyDesired. A pointer field stays nil
// when the device did not report that value, so a partial read never emits a
// zero temperature.
//
// The exact match is deliberate: the resource also lists sub-zones such as
// "Cool Select Fridge Zone", which have their own temperature and would
// otherwise overwrite the compartment's own reading with whichever item the
// device happens to list last.
func MapTemperatures(res map[string]any) samsung.TemperatureEvent {
	var ev samsung.TemperatureEvent
	for _, item := range items(res) {
		current := toFloat(item[samsung.KeyCurrent])
		desired := toFloat(item[samsung.KeyDesired])
		switch exactCompartmentOf(item) {
		case samsung.CompartmentFridge:
			if current != nil {
				ev.FridgeCurrent = current
			}
			if desired != nil {
				ev.FridgeSetpoint = desired
			}
		case samsung.CompartmentFreezer:
			if current != nil {
				ev.FreezerCurrent = current
			}
			if desired != nil {
				ev.FreezerSetpoint = desired
			}
		}
	}
	return ev
}

// MapDoors reads the /doors items into a DoorEvent. A door counts as open when
// its KeyOpenState equals StateOpen. A compartment usually has more than one
// door — a french-door fridge reports a left and a right one — so the states of
// all doors of a compartment are OR-ed: the compartment is open as soon as one
// of its doors is. AnyOpen is the same OR over every door that was read,
// independent of its compartment.
//
// A field stays nil when no door contributed to it: an item without a readable
// KeyOpenState is not a closed door but an unread one.
func MapDoors(res map[string]any) samsung.DoorEvent {
	var ev samsung.DoorEvent
	for _, item := range items(res) {
		open, ok := doorOpen(item)
		if !ok {
			continue
		}
		orInto(&ev.AnyOpen, open)
		switch doorCompartment(item) {
		case samsung.CompartmentFridge:
			orInto(&ev.FridgeOpen, open)
		case samsung.CompartmentFreezer:
			orInto(&ev.FreezerOpen, open)
		}
	}
	return ev
}

// doorOpen reads one door item's open state. ok is false when the item carries
// no open state at all, which keeps an unread door out of the OR.
func doorOpen(item map[string]any) (open bool, ok bool) {
	s := asString(item[samsung.KeyOpenState])
	if s == "" {
		return false, false
	}
	return s == samsung.StateOpen, true
}

// orInto ORs open into the flag behind p, allocating it on the first door that
// contributes. A flag that stays nil means no door was read for it.
func orInto(p **bool, open bool) {
	if *p == nil {
		v := open
		*p = &v
		return
	}
	**p = **p || open
}

// MapEnergy reads the instantaneous power and cumulative consumption from the
// /energy resource. Both fields stay nil when the device did not report them.
func MapEnergy(res map[string]any) samsung.EnergyEvent {
	return samsung.EnergyEvent{
		PowerW:       toFloat(res[samsung.KeyInstPower]),
		CumulativeWh: toFloat(res[samsung.KeyCumulativeEnergy]),
	}
}

// MapRefrigeration reads the rapid-cooling flags from the /refrigeration
// resource. A flag is true only when its value equals ValueOn, and nil when the
// resource carries no value for it — a missing key is an unknown state, not a
// reported "off".
func MapRefrigeration(res map[string]any) samsung.RefrigerationEvent {
	return samsung.RefrigerationEvent{
		RapidFridge:   onOff(res[samsung.KeyRapidFridge]),
		RapidFreezing: onOff(res[samsung.KeyRapidFreezing]),
	}
}

// MapIcemaker maps the ice maker resource. The status string is passed through
// as the appliance spells it; an absent key stays nil rather than becoming an
// empty string, so "the appliance did not report it" and "it reported nothing"
// do not read alike.
func MapIcemaker(res map[string]any) samsung.IcemakerEvent {
	ev := samsung.IcemakerEvent{On: onOff(res[samsung.KeyIceMakerState])}
	if s := asString(res[samsung.KeyIceMakerStatus]); s != "" {
		ev.Status = &s
	}
	return ev
}

// onOff turns an On/Off representation value into a *bool: nil when the value
// is absent or not a string, true only for ValueOn.
func onOff(v any) *bool {
	s := asString(v)
	if s == "" {
		return nil
	}
	b := s == samsung.ValueOn
	return &b
}

// doorCompartment classifies a door item. The /doors resource may omit the
// compartment description that /temperatures carries — a TP1X_REF_21K labels
// both items "Door" — so it falls back to the item id.
//
// MODEL FINDING: without a description, item id "0" is the freezer door and "1"
// the fridge door. That is the order /temperatures reports on the same
// appliance, and it was verified at the device by opening each door. It held
// the opposite way until then, which reported every open door as the other
// compartment.
//
// This is what one model reports, not a permanent fact: the id-to-compartment
// assignment differs between models, which is why the description is tried
// first and the id is only the fallback.
func doorCompartment(item map[string]any) string {
	if c := compartmentOf(item); c != "" {
		return c
	}
	switch asString(item[samsung.KeyID]) {
	case "0":
		return samsung.CompartmentFreezer
	case "1":
		return samsung.CompartmentFridge
	}
	return ""
}

// items returns the item maps inside res[KeyItems]. It tolerates a missing field
// and skips elements that are not maps, so a malformed representation yields an
// empty result rather than a panic.
func items(res map[string]any) []map[string]any {
	raw, ok := res[samsung.KeyItems].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// compartmentOf reads the compartment from an item's description, matched
// case-insensitively as a substring against the known fridge/freezer labels.
// It returns "" when the item names no known compartment.
//
// This is the right match for doors, whose descriptions name the door and not
// just the compartment ("Fridge Left Door"), and the wrong one for
// temperatures, where a sub-zone would be taken for the compartment itself; see
// exactCompartmentOf.
func compartmentOf(item map[string]any) string {
	desc := strings.ToLower(asString(item[samsung.KeyDescription]))
	switch {
	case strings.Contains(desc, samsung.CompartmentFreezer):
		return samsung.CompartmentFreezer
	case strings.Contains(desc, samsung.CompartmentFridge):
		return samsung.CompartmentFridge
	}
	return ""
}

// exactCompartmentOf returns the compartment an item describes itself as, and
// only that: the description has to be the compartment name, up to casing and
// surrounding space. Any additional wording ("Cool Select Fridge Zone") names a
// zone of a compartment rather than the compartment and yields "".
func exactCompartmentOf(item map[string]any) string {
	desc := strings.ToLower(strings.TrimSpace(asString(item[samsung.KeyDescription])))
	switch desc {
	case samsung.CompartmentFridge:
		return samsung.CompartmentFridge
	case samsung.CompartmentFreezer:
		return samsung.CompartmentFreezer
	}
	return ""
}

// asString returns v as a string, or "" when v is absent or not a string.
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// toFloat pulls a numeric value out of a CBOR-decoded representation into a
// *float64. A CBOR decoder may hand integers back as int64 or uint64 and reals
// as float64; some representations carry the number as a string. All of those
// convert; a missing or non-numeric value yields nil, never a zero, so the event
// mapping can leave the field unset.
func toFloat(v any) *float64 {
	switch n := v.(type) {
	case nil:
		return nil
	case float64:
		return &n
	case float32:
		f := float64(n)
		return &f
	case int:
		f := float64(n)
		return &f
	case int8:
		f := float64(n)
		return &f
	case int16:
		f := float64(n)
		return &f
	case int32:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	case uint:
		f := float64(n)
		return &f
	case uint8:
		f := float64(n)
		return &f
	case uint16:
		f := float64(n)
		return &f
	case uint32:
		f := float64(n)
		return &f
	case uint64:
		f := float64(n)
		return &f
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil
		}
		return &f
	default:
		return nil
	}
}
