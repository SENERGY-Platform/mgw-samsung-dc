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

// samsung-cli runs single functions of the connector against a real appliance.
// It reads the same environment variables as the connector and uses the same
// OCF transport, event mapping and command mapping, so what it prints is what
// the connector would emit.
//
// Only "set" writes to the appliance; nothing writes to the platform.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/config"
	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung"
	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung/mapping"
	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung/ocf"
)

const usage = `usage: samsung-cli [-json] <command> [args]

commands:
  uuid                       OCF client UUID the appliance has to authorize, and where its key is kept
  info                       read /oic/d: device id, name and model of the appliance
  resources                  read /oic/res: the resources the appliance offers
  get <path>                 read one OCF resource, e.g. /temperatures/vs/0
  read                       read every resource the connector uses, as the appliance returns it
  events [service...]        the events the connector would emit, per read service;
                             with service names only those, e.g. events doors
  set <service> <value>      write one set service (writes to the appliance)

set services: ` + setServices + `

Configuration comes from the connector's environment variables: OCF_HOST and
CONNECTOR_ID are required, OCF_PORT and CERT_PATH have the connector's defaults.
CERT_PATH holds the client key; the UUID changes when that key changes.
`

const setServices = samsung.SvcSetFridgeSetpoint + ", " + samsung.SvcSetFreezerSetpoint + ", " +
	samsung.SvcSetRapidFridge + ", " + samsung.SvcSetRapidFreezing + ", " + samsung.SvcSetIcemaker

// callTimeout bounds one OCF call, as the connector's ocfTimeout does. The DTLS
// handshake is lazy and only completes on the first request, so this is the
// budget for handshake and request together.
//
// A resource the client is not authorized for runs into it: the appliance
// answers the public resources of a client it does not know and drops the
// requests to the others without a response code, so an unauthorized read is a
// timeout and not a 4.03.
const callTimeout = 10 * time.Second

// resources are the resources the connector reads, in the order they are
// printed. /oic/d is included because the connector resolves the device id from
// it before it announces anything.
var resources = []string{
	"/oic/d",
	samsung.ResDeviceInfo,
	samsung.ResTemperatures,
	samsung.ResDoors,
	samsung.ResEnergy,
	samsung.ResRefrigeration,
	samsung.ResIcemaker,
}

func main() {
	asJson := flag.Bool("json", false, "print JSON instead of tables")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(flag.Arg(0), flag.Args()[1:], *asJson); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(command string, args []string, asJson bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// The CLI speaks to one appliance at a time. The connector may serve
	// several, so OCF_HOST may list several; the first entry is the one meant
	// here, and the rest are reached by giving OCF_HOST that single address.
	appliance := cfg.Appliances[0]
	if len(cfg.Appliances) > 1 {
		fmt.Fprintf(os.Stderr, "note: OCF_HOST lists %d appliances, using %s\n",
			len(cfg.Appliances), appliance.Addr())
	}
	client, err := ocf.New(appliance.Host, appliance.Port, cfg.CertPath, cfg.OcfClientUUID)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	switch command {
	case "uuid":
		return uuid(client, cfg, appliance, asJson)
	case "info":
		return info(client, asJson)
	case "resources":
		return listResources(client, asJson)
	case "get":
		if len(args) != 1 {
			return errors.New("get needs exactly one resource path")
		}
		return printResource(client, args[0])
	case "read":
		return readAll(client, asJson)
	case "events":
		return events(client, args, asJson)
	case "set":
		if len(args) != 2 {
			return fmt.Errorf("set needs a service and a value, one of: %s", setServices)
		}
		return set(client, args[0], args[1])
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

// uuid prints the client identity. It touches the network not at all: the UUID
// is OCF_CLIENT_UUID (the key in CERT_PATH is self-signed and not what the
// appliance checks), and it is what has to be authorized on the appliance
// before any read succeeds.
func uuid(client *ocf.Client, cfg *config.Config, appliance config.Appliance, asJson bool) error {
	if asJson {
		return printJson(map[string]string{
			"client_uuid": client.ClientUUID(),
			"cert_path":   cfg.CertPath,
			"ocf_host":    appliance.Addr(),
		})
	}
	fmt.Printf("client uuid: %s\n", client.ClientUUID())
	fmt.Printf("key and certificate: %s\n", cfg.CertPath)
	fmt.Printf("appliance: %s\n", appliance.Addr())
	return nil
}

// info reads /oic/d, the resource the connector resolves the device id from.
func info(client *ocf.Client, asJson bool) error {
	res, err := get(client, "/oic/d")
	if err != nil {
		return err
	}
	if asJson {
		return printJson(res)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "device id (di)\t%s\n", asString(res["di"]))
	fmt.Fprintf(w, "name (n)\t%s\n", asString(res["n"]))
	fmt.Fprintf(w, "model (dmno)\t%s\n", asString(res["dmno"]))
	fmt.Fprintf(w, "spec version (icv)\t%s\n", asString(res["icv"]))
	return w.Flush()
}

// printResource reads one resource and prints it as JSON, whatever it carries.
// It decodes without requiring a map so that a resource answering with an array
// prints instead of failing.
func printResource(client *ocf.Client, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	res, err := client.GetValue(ctx, path)
	if err != nil {
		return err
	}
	return printJson(res)
}

// listResources reads the discovery resource /oic/res and prints the links it
// carries: which resources the appliance offers, under which path and resource
// type.
//
// The listing is filtered by the appliance's access control list, so a resource
// missing from it is not evidence that the path is wrong: an unauthorized
// client is shown the onboarding resources and little else, while the vendor
// resources the connector reads appear only once its UUID is allowed to read
// them.
func listResources(client *ocf.Client, asJson bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	res, err := client.GetValue(ctx, "/oic/res")
	if err != nil {
		return err
	}
	if asJson {
		return printJson(res)
	}

	links := links(res)
	if len(links) == 0 {
		fmt.Println("no links in /oic/res; raw representation:")
		return printJson(res)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "path\tresource type\tinterfaces")
	for _, l := range links {
		fmt.Fprintf(w, "%s\t%s\t%s\n", asString(l["href"]), strings.Join(strs(l["rt"]), ", "), strings.Join(strs(l["if"]), ", "))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	offered := make(map[string]bool, len(links))
	for _, l := range links {
		offered[asString(l["href"])] = true
	}
	fmt.Println()
	fmt.Println("resources the connector reads:")
	missing := 0
	for _, path := range resources {
		state := "listed"
		if !offered[path] {
			state = "not listed"
			missing++
		}
		fmt.Printf("  %-28s %s\n", path, state)
	}
	if missing > 0 {
		fmt.Println()
		fmt.Println("A resource missing from the listing is withheld or absent; the listing")
		fmt.Println("does not tell the two apart. Only the onboarding resources being listed")
		fmt.Println("is what an unauthorized client sees.")
	}
	return nil
}

// links pulls the link objects out of an /oic/res representation. OCF allows
// both shapes: a flat array of links, and an array of collections that each
// carry their links under "links". Anything else yields no links, and the
// caller prints the raw representation instead of guessing.
func links(res any) []map[string]any {
	var out []map[string]any
	entries, ok := res.([]any)
	if !ok {
		return nil
	}
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if nested, ok := m["links"].([]any); ok {
			for _, l := range nested {
				if lm, ok := l.(map[string]any); ok {
					out = append(out, lm)
				}
			}
			continue
		}
		if _, ok := m["href"]; ok {
			out = append(out, m)
		}
	}
	return out
}

// strs renders a CBOR value that should be a string array for the table.
func strs(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if s := asString(v); s != "-" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		out = append(out, asString(e))
	}
	return out
}

// readAll reads every resource the connector uses. A resource that fails is
// reported and the others are still read: on a first run against a new
// appliance it is the set of resources that answer which is the result, and one
// missing resource must not hide the rest.
func readAll(client *ocf.Client, asJson bool) error {
	out := make(map[string]any, len(resources))
	var firstErr error
	for _, path := range resources {
		res, err := get(client, path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			out[path] = map[string]string{"error": err.Error()}
			if !asJson {
				fmt.Printf("\n-- %s\n%s\n", path, err)
			}
			continue
		}
		out[path] = res
		if !asJson {
			fmt.Printf("\n-- %s\n", path)
			if err := printJson(res); err != nil {
				return err
			}
		}
	}
	if asJson {
		if err := printJson(out); err != nil {
			return err
		}
	}
	return firstErr
}

// events prints what the connector would emit per read service: the mapped
// event for each polled resource. A nil field in the output means
// the appliance did not report that value — the mapping leaves it out rather
// than sending a zero.
//
// Named services limit the run to those. That is not only shorter output: a
// resource the client is not authorized for costs the full callTimeout, so
// asking for one service is the difference between one timeout and five.
func events(client *ocf.Client, services []string, asJson bool) error {
	type reader struct {
		service string
		path    string
		mapper  func(map[string]any) any
	}
	readers := []reader{
		{samsung.SvcTemperatures, samsung.ResTemperatures, func(r map[string]any) any { return mapping.MapTemperatures(r) }},
		{samsung.SvcDoors, samsung.ResDoors, func(r map[string]any) any { return mapping.MapDoors(r) }},
		{samsung.SvcEnergy, samsung.ResEnergy, func(r map[string]any) any { return mapping.MapEnergy(r) }},
		{samsung.SvcRefrigeration, samsung.ResRefrigeration, func(r map[string]any) any { return mapping.MapRefrigeration(r) }},
		{samsung.SvcIcemaker, samsung.ResIcemaker, func(r map[string]any) any { return mapping.MapIcemaker(r) }},
	}

	if len(services) > 0 {
		wanted := make(map[string]bool, len(services))
		for _, s := range services {
			wanted[s] = true
		}
		var selected []reader
		for _, r := range readers {
			if wanted[r.service] {
				selected = append(selected, r)
				delete(wanted, r.service)
			}
		}
		if len(wanted) > 0 {
			unknown := make([]string, 0, len(wanted))
			for s := range wanted {
				unknown = append(unknown, s)
			}
			sort.Strings(unknown)
			known := make([]string, 0, len(readers))
			for _, r := range readers {
				known = append(known, r.service)
			}
			return fmt.Errorf("unknown read service %s, one of: %s",
				strings.Join(unknown, ", "), strings.Join(known, ", "))
		}
		readers = selected
	}

	out := make(map[string]any, len(readers))
	var firstErr error
	for _, r := range readers {
		res, err := get(client, r.path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			out[r.service] = map[string]string{"error": err.Error()}
			if !asJson {
				fmt.Printf("\n-- %s (%s)\n%s\n", r.service, r.path, err)
			}
			continue
		}
		event := r.mapper(res)
		out[r.service] = event
		if !asJson {
			fmt.Printf("\n-- %s (%s)\n", r.service, r.path)
			if err := printJson(event); err != nil {
				return err
			}
		}
	}
	if asJson {
		if err := printJson(out); err != nil {
			return err
		}
	}
	return firstErr
}

// set writes one set service to the appliance. A command that is built against a
// representation gets a fresh read of that resource first, exactly as the
// connector builds it, so the write addresses what the appliance reported and
// is checked against what the appliance reported.
func set(client *ocf.Client, service string, value string) error {
	var representation map[string]any
	if resource := mapping.ReadResourceFor(service); resource != "" {
		var err error
		representation, err = get(client, resource)
		if err != nil {
			return fmt.Errorf("read %s before the write: %w", resource, err)
		}
	}

	path, body, err := mapping.BuildCommandFromRepresentation(service, input(value), representation)
	if err != nil {
		return err
	}
	fmt.Printf("post %s\n", path)
	if err := printJson(body); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if err := client.Post(ctx, path, body); err != nil {
		return err
	}
	fmt.Println("written")
	return nil
}

// input turns a command line argument into the value shape the command mapping
// expects. The mapping takes the SENERGY command input, which is a JSON value,
// so a numeric argument is passed as a number and everything else as a string.
func input(value string) any {
	var v any
	if err := json.Unmarshal([]byte(value), &v); err == nil {
		switch v.(type) {
		case float64, bool:
			return v
		}
	}
	return value
}

func get(client *ocf.Client, path string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return client.Get(ctx, path)
}

func printJson(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func asString(v any) string {
	if v == nil {
		return "-"
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprint(v)
	}
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
