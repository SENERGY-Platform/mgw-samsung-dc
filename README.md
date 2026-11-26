mgw-samsung-dc
==============

Device connector for the multi-gateway. Reads a Samsung refrigerator over OCF/CoAP in the local network and makes it available as an mgw device. No vendor cloud and no SmartThings account are involved; the gateway talks to the appliance directly.

The module definition lives in the shared module repository, in `main/samsung-dc/Modfile.yaml` of [mgw-module-repository](https://github.com/SENERGY-Platform/mgw-module-repository).

How it works
------------

- One connector instance serves every appliance `OCF_HOST` lists. Each becomes its own device under the device id it reports, with its own online state, its own poll cycle and its own commands; a fridge that is switched off does not affect the others.
- The connector opens a CoAP/DTLS session to the appliance and authenticates with a self-signed ECDSA P-256 client certificate. The OCF device UUID is derived from the public key of that certificate, so it stays the same as long as the key does.
- Each read resource is fetched and emitted as an event on its own interval. `POLL_INTERVAL` is the default for all of them; `POLL_INTERVAL_<SERVICE>` overrides one. Values the appliance did not report are left out of the payload instead of being sent as zero, so a renamed or missing field reads as "unknown" and not as "0 watts" or "door closed".
- Commands write a single OCF resource each.

Services
--------

| Local id | Direction | Content |
|---|---|---|
| `temperatures` | event | Current temperature and setpoint of fridge and freezer |
| `doors` | event | Open state per compartment and a combined "any door open" |
| `energy` | event | Instantaneous power in watts and the cumulative energy counter in watt-hours |
| `refrigeration` | event | Rapid fridge and rapid freezing flags |
| `icemaker` | event | Whether the ice maker is on, and what it reports it is doing |
| `set_fridge_setpoint` | request | New fridge setpoint |
| `set_freezer_setpoint` | request | New freezer setpoint |
| `set_rapid_fridge` | request | Switch rapid fridge on or off |
| `set_rapid_freezing` | request | Switch rapid freezing on or off |
| `set_icemaker` | request | Switch the ice maker on or off |

Compartments are matched by the description the appliance reports for a temperature item, not by a fixed item id, because the id-to-compartment assignment differs between models. The `/doors` resource of a TP1X_REF_21K labels both of its items `Door` and carries no compartment, so door items without a usable description fall back to the item id: `0` is the freezer door and `1` the fridge door, the same order `/temperatures` reports on that appliance, verified by opening each door. A model that does label its door items is matched by the description and not re-sorted by that order.

The cumulative energy counter is read from `x.com.samsung.da.cumulativePower`, not from the similarly named `cumulativeConsumption`. Two reads 39m56s apart on a TP1X_REF_21K gave 6627 and 6647 for the first, and 521 then 21 for the second: only the first rises, and its increase of 20 Wh matches a 30 W average between the 4 W and 48 W the same two reads report instantaneously. What `cumulativeConsumption` counts, and when it resets, is unknown, so it is not emitted.

The ice maker reports how it is operated. `set_icemaker` writes only to an appliance that reports the `toggle` scheme and is rejected otherwise, and it is rejected before the first successful `icemaker` read, because the scheme is then unknown. The ice-making status is passed through as the appliance spells it, for example `ICESTATUS_STOP`; the other values it can take are not known, so no "is making ice" flag is derived from it. The ice type the appliance reports (`NORMAL`) is not carried and not written: whether it is writable is untested.

Configuration
-------------

| Variable | Config name | Default | Meaning |
|---|---|---|---|
| `CONNECTOR_ID` | Connector ID | required | Unique id of this connector installation |
| `OCF_HOST` | Appliance IPs | required | Addresses of the refrigerators in the local network, comma-separated. An entry may carry its own port: `10.0.0.5, 10.0.0.6:49156`. An IPv6 address needs brackets to carry one: `[fe80::1]:49156` |
| `OCF_PORT` | OCF Port | `49155` | OCF secure port of an `OCF_HOST` entry that names none |
| `DEVICE_TYPE_ID` | Device Type | see below | SENERGY device type the device is announced with |
| `POLL_INTERVAL` | Poll Interval | `60s` | Default interval of every read service, a Go duration such as `60s` or `1m` |
| `POLL_INTERVAL_DOORS` | Poll Interval Doors | `POLL_INTERVAL` | Interval of the `doors` service; the module sets `10s` |
| `POLL_INTERVAL_TEMPERATURES` | Poll Interval Temperatures | `POLL_INTERVAL` | Interval of the `temperatures` service |
| `POLL_INTERVAL_ENERGY` | Poll Interval Energy | `POLL_INTERVAL` | Interval of the `energy` service |
| `POLL_INTERVAL_REFRIGERATION` | Poll Interval Rapid Cooling | `POLL_INTERVAL` | Interval of the `refrigeration` service; the module sets `300s` |
| `POLL_INTERVAL_ICEMAKER` | Poll Interval Ice Maker | `POLL_INTERVAL` | Interval of the `icemaker` service |
| `MGW_MQTT_BROKER` | | `tcp://localhost:1883` | mgw broker; the module dependency sets it to the broker module |
| `MGW_MQTT_USER`, `MGW_MQTT_PW` | MGW MQTT User / Password | empty | Broker credentials, if the broker requires them |
| `DEBUG` | Debug | `false` | Debug log level |
| `OCF_CLIENT_UUID` | | `ab0b0ac4-…b2f9` | UUID the client certificate presents; the appliance authorizes by this string, not by the key |

The read services differ in how fast a stale value matters: a door that was opened a minute ago is no longer news, while the rapid-cooling flags change almost only in response to a command. The module definition therefore polls `doors` every 10 seconds and `refrigeration` every 300 seconds, and leaves the other two at `POLL_INTERVAL`. Raising the door rate raises the request rate on the appliance by the same factor; whether the appliance tolerates that over long periods has not been measured.

The offline probe runs at the fastest configured interval, but never more often than every 30 seconds. It only runs while the device is announced offline, where every attempt is a full DTLS handshake into a timeout.

Without `DEVICE_TYPE_ID` the connector starts and logs a warning; the devices are then announced without a device type and the platform cannot interpret their services.

Several appliances
------------------

`OCF_HOST` is a list, and one process serves all of it. The MGW library registers each service once and calls it for every device it knows, so a second refrigerator costs a second OCF session rather than a second deployment of the module.

What is shared and what is not follows from that. Device type, poll intervals and the client certificate are connector-wide: the appliances are the same model on the same LAN, authorized by the same client identity. Everything that describes a single fridge is per appliance — the device id it reports, its name, its online state, the failure count behind that state, and the last readings its commands are built against. A setpoint is therefore always checked against the range the appliance it is addressed to reported, never against another one's. A setpoint for a compartment the appliance has not reported is refused rather than written: there would be neither a range to check the value against nor the appliance's own way of addressing that compartment, and a read of a resource with an empty body succeeds, so a representation without the compartment is reachable without any transport error.

Each appliance announces itself as soon as it answers. One that is unreachable at startup keeps retrying in the background at its own pace and announces later; it holds back neither the connector nor the other appliances. The device name is what the appliance reports under `/oic/d`; an appliance that reports none is labelled `Samsung Refrigerator (<address>)`, so the list stays readable in the UI.

`OCF_HOST` is checked at startup rather than at the first dial: an entry that is not a host — a list separated with a space or a semicolon instead of a comma, a port appended twice, a duplicate address — is rejected and the connector does not start. Without that check such an entry passes as a hostname, and the only sign of the missing device is a retry warning every fifteen seconds. An IPv6 address may be written bare or in brackets, and needs the brackets to carry a port: `[fe80::1]:49156`.

Two entries that resolve to the same OCF device id — one refrigerator reachable under two addresses — are not announced twice. The second is dropped with a warning, because two devices under one id overwrite each other's state and events in the device-manager.

The library polls all devices of one service in parallel and waits for the slowest before the next tick of that service. `ocfTimeout` bounds each read at ten seconds, so a dead appliance delays a service by at most that; see the note in `pkg/samsung/ocf` for the one handshake case no context bounds.

The CLI speaks to one appliance at a time. With several in `OCF_HOST` it uses the first and says so; to reach another, give `OCF_HOST` that single address.

Device type
-----------

The services above match the device type `urn:infai:ses:device-type:cb2cfd90-1ab8-4a6d-a457-7d84e0d4b1fc` on SENERGY. It is the default of the `deviceTypeId` config and only needs to be changed if the appliance is modelled by a different device type. It applies to every appliance of one connector, so refrigerators modelled by different device types need one deployment each.

Access on the appliance
-----------------------

The appliance authorizes a client by the UUID string in its client certificate subject, not by the certificate's key or chain (TizenRT iotivity matches the subject with `memmem(subject, "uuid:")`). `OCF_CLIENT_UUID` is that UUID. It defaults to the Samsung service identity that the factory access control list of these refrigerators already grants full access to, so no on-device onboarding is needed; the connector presents it and reads succeed.

The certificate is self-signed with a fresh key, written to `CERT_PATH` on first use and only re-read afterwards. The key is irrelevant to authorization here — the appliance never checks it — but it is kept persistent so the TLS identity is stable across restarts. The appliance sends a `CertificateRequest` listing acceptable CAs that our self-signed certificate does not match; the connector presents the certificate regardless, because the device gates on the subject UUID, not the chain.

A client whose UUID is not in the access control list still completes the DTLS handshake, and the public resources answer — `/oic/d` returns the device id, `/oic/res` lists a handful of onboarding resources — while a read of `/temperatures/vs/0` or any other vendor resource runs into its timeout: the appliance drops a request it does not allow instead of answering 4.03. `/oic/res` never lists the vendor resources, authorized or not; they are read by their fixed paths.

Trying it against the appliance
-------------------------------

`scripts/try-samsung.sh` asks for the address of the appliance once, stores it in `.local/samsung.env` (git-ignored, mode 600) and runs the CLI read-only: the client UUID, `/oic/d`, the resources the appliance offers, every resource the connector reads, and the events the connector would emit from them. With `credctl` it also looks up the configured device type on the platform and compares its service ids with the ones the connector registers. Results land in `.local/out/<timestamp>/`.

The client identity it uses lives in `.local/client-cert` and is reused by every run, so the UUID stays the one that was authorized on the appliance. Deleting that directory produces a new UUID and the appliance stops answering.

Single functions: `go run ./cmd/samsung-cli` with the same environment variables as the connector. `set <service> <value>` is the only command that writes, and the script never calls it.
