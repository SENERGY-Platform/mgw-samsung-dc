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
	"reflect"
	"testing"

	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung"
)

// fp returns a pointer to f, for building expected pointer-valued event fields.
func fp(f float64) *float64 { return &f }

// bp returns a pointer to b, for the pointer-valued door and rapid-mode flags.
func bp(b bool) *bool { return &b }

func TestToFloat(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want *float64
	}{
		{"nil", nil, nil},
		{"absent key reads as nil", map[string]any{}["missing"], nil},
		{"float64", float64(3.5), fp(3.5)},
		{"float32", float32(1.5), fp(1.5)},
		{"int", int(5), fp(5)},
		{"int8", int8(-4), fp(-4)},
		{"int16", int16(300), fp(300)},
		{"int32", int32(-18), fp(-18)},
		{"int64", int64(4), fp(4)},
		{"uint", uint(6), fp(6)},
		{"uint8", uint8(250), fp(250)},
		{"uint16", uint16(400), fp(400)},
		{"uint32", uint32(70000), fp(70000)},
		{"uint64", uint64(7), fp(7)},
		{"numeric string", "12.5", fp(12.5)},
		{"numeric string with space", "  9  ", fp(9)},
		{"negative numeric string", "-18", fp(-18)},
		{"empty string", "", nil},
		{"non-numeric string", "abc", nil},
		{"bool is not numeric", true, nil},
		{"map is not numeric", map[string]any{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := toFloat(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("toFloat(%#v) = %v, want %v", c.in, deref(got), deref(c.want))
			}
		})
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestMapTemperatures(t *testing.T) {
	cases := []struct {
		name string
		res  map[string]any
		want samsung.TemperatureEvent
	}{
		{
			name: "both compartments, mixed number types",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "fridge",
						samsung.KeyCurrent:     int64(4),
						samsung.KeyDesired:     float64(3),
					},
					map[string]any{
						samsung.KeyDescription: "Freezer",
						samsung.KeyCurrent:     "-18",
						samsung.KeyDesired:     uint64(20), // stored as magnitude by some encoders
					},
				},
			},
			want: samsung.TemperatureEvent{
				FridgeCurrent:   fp(4),
				FridgeSetpoint:  fp(3),
				FreezerCurrent:  fp(-18),
				FreezerSetpoint: fp(20),
			},
		},
		{
			name: "missing desired leaves setpoint nil",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "FRIDGE",
						samsung.KeyCurrent:     float64(5),
					},
				},
			},
			want: samsung.TemperatureEvent{FridgeCurrent: fp(5)},
		},
		{
			// A sub-zone names a compartment but is not one: it carries its own
			// temperature and must not be read as the compartment's.
			name: "sub-zone of a compartment is ignored",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "Cool Select Fridge Zone",
						samsung.KeyCurrent:     int64(2),
					},
				},
			},
			want: samsung.TemperatureEvent{},
		},
		{
			// Defect 2: the sub-zone used to overwrite the compartment because it
			// is listed after it.
			name: "sub-zone does not overwrite its compartment",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "fridge",
						samsung.KeyCurrent:     float64(4),
						samsung.KeyDesired:     float64(3),
					},
					map[string]any{
						samsung.KeyDescription: "Cool Select Fridge Zone",
						samsung.KeyCurrent:     float64(-1),
						samsung.KeyDesired:     float64(-2),
					},
				},
			},
			want: samsung.TemperatureEvent{FridgeCurrent: fp(4), FridgeSetpoint: fp(3)},
		},
		{
			name: "compartment name with surrounding space still matches",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "  Freezer  ",
						samsung.KeyCurrent:     float64(-18),
					},
				},
			},
			want: samsung.TemperatureEvent{FreezerCurrent: fp(-18)},
		},
		{
			name: "unknown compartment is ignored",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "pantry",
						samsung.KeyCurrent:     int64(8),
					},
				},
			},
			want: samsung.TemperatureEvent{},
		},
		{
			name: "no items yields all nil",
			res:  map[string]any{},
			want: samsung.TemperatureEvent{},
		},
		{
			name: "items of wrong element type are skipped",
			res: map[string]any{
				samsung.KeyItems: []any{"not a map", 42},
			},
			want: samsung.TemperatureEvent{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MapTemperatures(c.res)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("MapTemperatures() = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestMapDoors(t *testing.T) {
	cases := []struct {
		name string
		res  map[string]any
		want samsung.DoorEvent
	}{
		{
			name: "descriptions present, fridge open",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "fridge",
						samsung.KeyOpenState:   samsung.StateOpen,
					},
					map[string]any{
						samsung.KeyDescription: "freezer",
						samsung.KeyOpenState:   samsung.StateClose,
					},
				},
			},
			want: samsung.DoorEvent{FridgeOpen: bp(true), FreezerOpen: bp(false), AnyOpen: bp(true)},
		},
		{
			// Verified at a TP1X_REF_21K, which labels both door items "Door":
			// id 0 is the freezer door, id 1 the fridge door, the same order
			// /temperatures reports.
			name: "fallback to id when no description",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyID:        "0",
						samsung.KeyOpenState: samsung.StateClose,
					},
					map[string]any{
						samsung.KeyID:        "1",
						samsung.KeyOpenState: samsung.StateOpen,
					},
				},
			},
			want: samsung.DoorEvent{FridgeOpen: bp(true), FreezerOpen: bp(false), AnyOpen: bp(true)},
		},
		{
			// The description wins over the id, so a model that labels its door
			// items is not re-sorted by the TP1X id order.
			name: "description wins over a contradicting id",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyID:          "0",
						samsung.KeyDescription: "Fridge",
						samsung.KeyOpenState:   samsung.StateOpen,
					},
					map[string]any{
						samsung.KeyID:          "1",
						samsung.KeyDescription: "Freezer",
						samsung.KeyOpenState:   samsung.StateClose,
					},
				},
			},
			want: samsung.DoorEvent{FridgeOpen: bp(true), FreezerOpen: bp(false), AnyOpen: bp(true)},
		},
		{
			name: "all closed yields AnyOpen false",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "fridge",
						samsung.KeyOpenState:   samsung.StateClose,
					},
					map[string]any{
						samsung.KeyDescription: "freezer",
						samsung.KeyOpenState:   samsung.StateClose,
					},
				},
			},
			want: samsung.DoorEvent{FridgeOpen: bp(false), FreezerOpen: bp(false), AnyOpen: bp(false)},
		},
		{
			name: "unclassified open door still sets AnyOpen",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "door-in-door",
						samsung.KeyOpenState:   samsung.StateOpen,
					},
				},
			},
			want: samsung.DoorEvent{AnyOpen: bp(true)},
		},
		{
			name: "no items leaves every flag nil",
			res:  map[string]any{},
			want: samsung.DoorEvent{},
		},
		{
			// Defect 3: a renamed or missing open-state key is an unread door, not
			// a closed one.
			name: "item without an open state leaves every flag nil",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{samsung.KeyDescription: "fridge"},
				},
			},
			want: samsung.DoorEvent{},
		},
		{
			// Defect 1: a compartment with several doors is open as soon as one of
			// them is, whatever order the device lists them in.
			name: "several doors per compartment are OR-ed",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "Fridge Left Door",
						samsung.KeyOpenState:   samsung.StateOpen,
					},
					map[string]any{
						samsung.KeyDescription: "Fridge Right Door",
						samsung.KeyOpenState:   samsung.StateClose,
					},
					map[string]any{
						samsung.KeyDescription: "Freezer Door",
						samsung.KeyOpenState:   samsung.StateClose,
					},
				},
			},
			want: samsung.DoorEvent{FridgeOpen: bp(true), FreezerOpen: bp(false), AnyOpen: bp(true)},
		},
		{
			name: "open door before a closed one of the same compartment",
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "Freezer Upper Door",
						samsung.KeyOpenState:   samsung.StateClose,
					},
					map[string]any{
						samsung.KeyDescription: "Freezer Lower Door",
						samsung.KeyOpenState:   samsung.StateOpen,
					},
				},
			},
			want: samsung.DoorEvent{FreezerOpen: bp(true), AnyOpen: bp(true)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MapDoors(c.res)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("MapDoors() = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestMapEnergy(t *testing.T) {
	cases := []struct {
		name string
		res  map[string]any
		want samsung.EnergyEvent
	}{
		{
			name: "both values, mixed types",
			res: map[string]any{
				samsung.KeyInstPower:        float64(120.5),
				samsung.KeyCumulativeEnergy: int64(34000),
			},
			want: samsung.EnergyEvent{PowerW: fp(120.5), CumulativeWh: fp(34000)},
		},
		{
			name: "missing power leaves it nil",
			res: map[string]any{
				samsung.KeyCumulativeEnergy: "34000",
			},
			want: samsung.EnergyEvent{CumulativeWh: fp(34000)},
		},
		{
			name: "empty resource yields all nil",
			res:  map[string]any{},
			want: samsung.EnergyEvent{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MapEnergy(c.res)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("MapEnergy() = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestMapRefrigeration(t *testing.T) {
	cases := []struct {
		name string
		res  map[string]any
		want samsung.RefrigerationEvent
	}{
		{
			name: "both on",
			res: map[string]any{
				samsung.KeyRapidFridge:   samsung.ValueOn,
				samsung.KeyRapidFreezing: samsung.ValueOn,
			},
			want: samsung.RefrigerationEvent{RapidFridge: bp(true), RapidFreezing: bp(true)},
		},
		{
			name: "fridge on, freezing off",
			res: map[string]any{
				samsung.KeyRapidFridge:   samsung.ValueOn,
				samsung.KeyRapidFreezing: samsung.ValueOff,
			},
			want: samsung.RefrigerationEvent{RapidFridge: bp(true), RapidFreezing: bp(false)},
		},
		{
			// Defect 3: a key the device spells differently must read as unknown,
			// not as a reported "off".
			name: "missing keys stay nil",
			res:  map[string]any{},
			want: samsung.RefrigerationEvent{},
		},
		{
			name: "one missing key leaves only that flag nil",
			res: map[string]any{
				samsung.KeyRapidFreezing: samsung.ValueOff,
			},
			want: samsung.RefrigerationEvent{RapidFreezing: bp(false)},
		},
		{
			name: "non-string value reads as unknown",
			res: map[string]any{
				samsung.KeyRapidFridge: 1,
			},
			want: samsung.RefrigerationEvent{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MapRefrigeration(c.res)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("MapRefrigeration() = %+v, want %+v", got, c.want)
			}
		})
	}
}

// icemakerRepresentation is the ice maker resource as a TP1X_REF_21K reports it.
func icemakerRepresentation() map[string]any {
	return map[string]any{
		samsung.KeyIceMakerStatus:        "ICESTATUS_STOP",
		"x.com.samsung.da.iceMaker.name": "ICE_MAKER",
		samsung.KeyIceMakerState:         "On",
		samsung.KeyIceMakerType:          "toggle",
		samsung.KeyIceTypeDesired:        "NORMAL",
	}
}

func TestMapIcemaker(t *testing.T) {
	ev := MapIcemaker(icemakerRepresentation())
	if ev.On == nil || !*ev.On {
		t.Errorf("On = %v, want true", ev.On)
	}
	if ev.Status == nil || *ev.Status != "ICESTATUS_STOP" {
		t.Errorf("Status = %v, want ICESTATUS_STOP", ev.Status)
	}
}

func TestMapIcemakerOff(t *testing.T) {
	res := icemakerRepresentation()
	res[samsung.KeyIceMakerState] = "Off"
	ev := MapIcemaker(res)
	if ev.On == nil || *ev.On {
		t.Errorf("On = %v, want false", ev.On)
	}
}

func TestMapIcemakerMissingKeysStayNil(t *testing.T) {
	ev := MapIcemaker(map[string]any{})
	if ev.On != nil {
		t.Errorf("On = %v, want nil for a resource without the state key", ev.On)
	}
	if ev.Status != nil {
		t.Errorf("Status = %v, want nil for a resource without the status key", ev.Status)
	}
}

// TestMapEnergyIgnoresCumulativeConsumption pins which of the two cumulative
// keys is emitted. The appliance reports both; only cumulativePower rises. Two
// reads 39m56s apart on a TP1X_REF_21K gave cumulativePower 6627 then 6647, and
// cumulativeConsumption 521 then 21 — the second fell by 500, so reading it as
// the energy counter makes a consumer's difference negative.
func TestMapEnergyIgnoresCumulativeConsumption(t *testing.T) {
	ev := MapEnergy(map[string]any{
		samsung.KeyInstPower:              "48",
		samsung.KeyCumulativeEnergy:       "6647",
		samsung.KeyCumulativeConsumption:  "21",
		"x.com.samsung.da.cumulativeUnit": "Wh",
	})
	if ev.CumulativeWh == nil || *ev.CumulativeWh != 6647 {
		t.Errorf("CumulativeWh = %v, want 6647 from cumulativePower", ev.CumulativeWh)
	}
	if ev.PowerW == nil || *ev.PowerW != 48 {
		t.Errorf("PowerW = %v, want 48", ev.PowerW)
	}
}

func TestMapEnergyWithoutCumulativePowerStaysNil(t *testing.T) {
	// cumulativeConsumption alone must not stand in for the counter.
	ev := MapEnergy(map[string]any{samsung.KeyCumulativeConsumption: "21"})
	if ev.CumulativeWh != nil {
		t.Errorf("CumulativeWh = %v, want nil", ev.CumulativeWh)
	}
}
