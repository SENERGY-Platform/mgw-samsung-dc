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
	"strings"
	"testing"

	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung"
)

// TestBuildCommand covers the services that address no compartment. A setpoint
// is not among them: without the representation it needs it is refused, which
// TestBuildCommandErrors covers.
func TestBuildCommand(t *testing.T) {
	cases := []struct {
		name     string
		service  string
		input    any
		wantPath string
		wantBody map[string]any
	}{
		{
			name:     "rapid fridge on from bool",
			service:  samsung.SvcSetRapidFridge,
			input:    true,
			wantPath: samsung.ResRefrigeration,
			wantBody: map[string]any{samsung.KeyRapidFridge: samsung.ValueOn},
		},
		{
			name:     "rapid fridge off from bool",
			service:  samsung.SvcSetRapidFridge,
			input:    false,
			wantPath: samsung.ResRefrigeration,
			wantBody: map[string]any{samsung.KeyRapidFridge: samsung.ValueOff},
		},
		{
			name:     "rapid freezing on from string",
			service:  samsung.SvcSetRapidFreezing,
			input:    "On",
			wantPath: samsung.ResRefrigeration,
			wantBody: map[string]any{samsung.KeyRapidFreezing: samsung.ValueOn},
		},
		{
			name:     "rapid freezing off from mixed-case string",
			service:  samsung.SvcSetRapidFreezing,
			input:    "off",
			wantPath: samsung.ResRefrigeration,
			wantBody: map[string]any{samsung.KeyRapidFreezing: samsung.ValueOff},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, body, err := BuildCommand(c.service, c.input)
			if err != nil {
				t.Fatalf("BuildCommand(%q, %#v) returned error: %v", c.service, c.input, err)
			}
			if path != c.wantPath {
				t.Errorf("path = %q, want %q", path, c.wantPath)
			}
			if !reflect.DeepEqual(body, c.wantBody) {
				t.Errorf("body = %#v, want %#v", body, c.wantBody)
			}
		})
	}
}

// readTemperatures is a /temperatures representation as the device reports it:
// both compartments with an id, a range and a sub-zone that names a compartment
// without being one.
func readTemperatures() map[string]any {
	return map[string]any{
		samsung.KeyItems: []any{
			map[string]any{
				samsung.KeyID:          "0",
				samsung.KeyDescription: "Fridge",
				samsung.KeyCurrent:     float64(4),
				samsung.KeyDesired:     float64(3),
				samsung.KeyMinimum:     float64(1),
				samsung.KeyMaximum:     float64(7),
			},
			map[string]any{
				samsung.KeyID:          "1",
				samsung.KeyDescription: "Freezer",
				samsung.KeyCurrent:     float64(-18),
				samsung.KeyDesired:     float64(-18),
				samsung.KeyMinimum:     float64(-23),
				samsung.KeyMaximum:     float64(-15),
			},
			map[string]any{
				samsung.KeyID:          "2",
				samsung.KeyDescription: "Cool Select Fridge Zone",
				samsung.KeyMinimum:     float64(-5),
				samsung.KeyMaximum:     float64(0),
			},
		},
	}
}

func TestBuildCommandFromRepresentation(t *testing.T) {
	cases := []struct {
		name     string
		service  string
		input    any
		res      map[string]any
		wantBody map[string]any
	}{
		{
			// Defect 4: the write addresses the item the read reported, with the
			// device's own id and spelling, instead of a guessed description.
			name:    "fridge setpoint addressed by the read id",
			service: samsung.SvcSetFridgeSetpoint,
			input:   float64(5),
			res:     readTemperatures(),
			wantBody: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyID:          "0",
						samsung.KeyDescription: "Fridge",
						samsung.KeyDesired:     float64(5),
					},
				},
			},
		},
		{
			name:    "freezer setpoint addressed by the read id",
			service: samsung.SvcSetFreezerSetpoint,
			input:   "-20",
			res:     readTemperatures(),
			wantBody: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyID:          "1",
						samsung.KeyDescription: "Freezer",
						samsung.KeyDesired:     float64(-20),
					},
				},
			},
		},
		{
			name:    "value on the reported bound is accepted",
			service: samsung.SvcSetFridgeSetpoint,
			input:   float64(7),
			res:     readTemperatures(),
			wantBody: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyID:          "0",
						samsung.KeyDescription: "Fridge",
						samsung.KeyDesired:     float64(7),
					},
				},
			},
		},
		{
			name:    "item without an id is addressed by its read description",
			service: samsung.SvcSetFridgeSetpoint,
			input:   int64(4),
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{samsung.KeyDescription: "FRIDGE"},
				},
			},
			wantBody: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyDescription: "FRIDGE",
						samsung.KeyDesired:     float64(4),
					},
				},
			},
		},
		{
			// The device reported this compartment, so the write addresses what
			// it reported; it just named no bounds for it. That is the device's
			// own statement, not a missing reading.
			name:    "item the device reported without bounds is written unchecked",
			service: samsung.SvcSetFridgeSetpoint,
			input:   float64(999),
			res: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{samsung.KeyID: "0", samsung.KeyDescription: "fridge"},
				},
			},
			wantBody: map[string]any{
				samsung.KeyItems: []any{
					map[string]any{
						samsung.KeyID:          "0",
						samsung.KeyDescription: "fridge",
						samsung.KeyDesired:     float64(999),
					},
				},
			},
		},
		{
			name:     "rapid mode ignores the representation",
			service:  samsung.SvcSetRapidFridge,
			input:    true,
			res:      readTemperatures(),
			wantBody: map[string]any{samsung.KeyRapidFridge: samsung.ValueOn},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, body, err := BuildCommandFromRepresentation(c.service, c.input, c.res)
			if err != nil {
				t.Fatalf("BuildCommandFromRepresentation(%q, %#v) returned error: %v", c.service, c.input, err)
			}
			wantPath := samsung.ResTemperatures
			if c.service == samsung.SvcSetRapidFridge || c.service == samsung.SvcSetRapidFreezing {
				wantPath = samsung.ResRefrigeration
			}
			if path != wantPath {
				t.Errorf("path = %q, want %q", path, wantPath)
			}
			if !reflect.DeepEqual(body, c.wantBody) {
				t.Errorf("body = %#v, want %#v", body, c.wantBody)
			}
		})
	}
}

// TestBuildCommandRangeErrors covers defect 4: a setpoint outside the range the
// device reported is refused instead of written.
func TestBuildCommandRangeErrors(t *testing.T) {
	cases := []struct {
		name    string
		service string
		input   any
	}{
		{"fridge setpoint above the maximum", samsung.SvcSetFridgeSetpoint, float64(999)},
		{"fridge setpoint below the minimum", samsung.SvcSetFridgeSetpoint, float64(-40)},
		{"freezer setpoint above the maximum", samsung.SvcSetFreezerSetpoint, float64(0)},
		{"freezer setpoint below the minimum", samsung.SvcSetFreezerSetpoint, "-50"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, body, err := BuildCommandFromRepresentation(c.service, c.input, readTemperatures())
			if err == nil {
				t.Fatalf("BuildCommandFromRepresentation(%q, %#v) = (%q, %#v, nil), want error", c.service, c.input, path, body)
			}
			if path != "" || body != nil {
				t.Errorf("on error want empty path and nil body, got (%q, %#v)", path, body)
			}
		})
	}
}

// TestSetpointWithoutAReportedItem: an unchecked setpoint sent to a guessed
// address must never leave this package. An OCF read of a resource with an
// empty body succeeds and yields an empty map, so a representation that carries
// no item for the compartment is reachable without any transport error and is
// refused here.
func TestSetpointWithoutAReportedItem(t *testing.T) {
	cases := []struct {
		name string
		res  map[string]any
	}{
		{"nil representation", nil},
		{"empty representation", map[string]any{}},
		{"representation with an empty items array", map[string]any{samsung.KeyItems: []any{}}},
		{"representation naming another compartment only",
			map[string]any{samsung.KeyItems: []any{map[string]any{samsung.KeyDescription: "pantry"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, service := range []string{samsung.SvcSetFridgeSetpoint, samsung.SvcSetFreezerSetpoint} {
				path, body, err := BuildCommandFromRepresentation(service, int64(3), c.res)
				if err == nil {
					t.Errorf("%s = (%q, %#v, nil), want error", service, path, body)
				}
				if path != "" || body != nil {
					t.Errorf("on error want empty path and nil body, got (%q, %#v)", path, body)
				}
			}
		})
	}
}

func TestBuildCommandErrors(t *testing.T) {
	cases := []struct {
		name    string
		service string
		input   any
	}{
		{"unknown service id", "set_something", true},
		{"empty service id", "", true},
		{"rapid fridge with non on/off string", samsung.SvcSetRapidFridge, "maybe"},
		{"rapid fridge with numeric input", samsung.SvcSetRapidFridge, 1},
		{"rapid freezing with nil input", samsung.SvcSetRapidFreezing, nil},
		{"fridge setpoint with non-numeric string", samsung.SvcSetFridgeSetpoint, "warm"},
		{"fridge setpoint with nil input", samsung.SvcSetFridgeSetpoint, nil},
		{"freezer setpoint with bool input", samsung.SvcSetFreezerSetpoint, true},
		// Without a representation there is neither a range to check the value
		// against nor the appliance's own way of addressing the compartment.
		{"fridge setpoint without a representation", samsung.SvcSetFridgeSetpoint, int64(3)},
		{"freezer setpoint without a representation", samsung.SvcSetFreezerSetpoint, "-18.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, body, err := BuildCommand(c.service, c.input)
			if err == nil {
				t.Fatalf("BuildCommand(%q, %#v) = (%q, %#v, nil), want error", c.service, c.input, path, body)
			}
			if path != "" || body != nil {
				t.Errorf("on error want empty path and nil body, got (%q, %#v)", path, body)
			}
		})
	}
}

func TestToOnOff(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		want    string
		wantErr bool
	}{
		{"bool true", true, samsung.ValueOn, false},
		{"bool false", false, samsung.ValueOff, false},
		{"string On", "On", samsung.ValueOn, false},
		{"string off lowercase", "off", samsung.ValueOff, false},
		{"string with spaces", "  ON  ", samsung.ValueOn, false},
		{"invalid string", "enable", "", true},
		{"numeric input", 0, "", true},
		{"nil input", nil, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := toOnOff(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("toOnOff(%#v) = %q, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("toOnOff(%#v) unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("toOnOff(%#v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestToNumber(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		want    float64
		wantErr bool
	}{
		{"float64", float64(3.5), 3.5, false},
		{"int64", int64(4), 4, false},
		{"uint64", uint64(7), 7, false},
		{"numeric string", "-18", -18, false},
		{"non-numeric string", "cold", 0, true},
		{"bool", true, 0, true},
		{"nil", nil, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := toNumber(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("toNumber(%#v) = %v, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("toNumber(%#v) unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("toNumber(%#v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestBuildIcemaker(t *testing.T) {
	res := map[string]any{
		samsung.KeyIceMakerState: "Off",
		samsung.KeyIceMakerType:  samsung.IceMakerTypeToggle,
	}
	for _, c := range []struct {
		input any
		want  string
	}{
		{true, samsung.ValueOn},
		{false, samsung.ValueOff},
		{"on", samsung.ValueOn},
		{" OFF ", samsung.ValueOff},
	} {
		path, body, err := BuildCommandFromRepresentation(samsung.SvcSetIcemaker, c.input, res)
		if err != nil {
			t.Fatalf("input %#v returned error: %v", c.input, err)
		}
		if path != samsung.ResIcemaker {
			t.Errorf("path = %q, want %q", path, samsung.ResIcemaker)
		}
		if got := body[samsung.KeyIceMakerState]; got != c.want {
			t.Errorf("input %#v wrote %v, want %q", c.input, got, c.want)
		}
		if len(body) != 1 {
			t.Errorf("body = %#v, want only the state key", body)
		}
	}
}

func TestBuildIcemakerWithoutRepresentationIsRejected(t *testing.T) {
	if _, _, err := BuildCommand(samsung.SvcSetIcemaker, true); err == nil {
		t.Fatal("expected an error without an ice maker reading")
	}
}

func TestBuildIcemakerRejectsUnknownType(t *testing.T) {
	res := map[string]any{
		samsung.KeyIceMakerState: "On",
		samsung.KeyIceMakerType:  "slider",
	}
	_, _, err := BuildCommandFromRepresentation(samsung.SvcSetIcemaker, true, res)
	if err == nil {
		t.Fatal("expected an error for an ice maker type the write form is unknown for")
	}
	if !strings.Contains(err.Error(), "slider") {
		t.Errorf("error %q does not name the reported type", err)
	}
}

func TestBuildIcemakerRejectsInvalidInput(t *testing.T) {
	res := map[string]any{samsung.KeyIceMakerType: samsung.IceMakerTypeToggle}
	if _, _, err := BuildCommandFromRepresentation(samsung.SvcSetIcemaker, 7, res); err == nil {
		t.Fatal("expected an error for a numeric ice maker input")
	}
}

func TestReadResourceFor(t *testing.T) {
	want := map[string]string{
		samsung.SvcSetFridgeSetpoint:  samsung.ResTemperatures,
		samsung.SvcSetFreezerSetpoint: samsung.ResTemperatures,
		samsung.SvcSetIcemaker:        samsung.ResIcemaker,
		samsung.SvcSetRapidFridge:     "",
		samsung.SvcSetRapidFreezing:   "",
	}
	for svc, w := range want {
		if got := ReadResourceFor(svc); got != w {
			t.Errorf("ReadResourceFor(%q) = %q, want %q", svc, got, w)
		}
	}
}
