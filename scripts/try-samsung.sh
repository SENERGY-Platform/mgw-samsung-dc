#!/usr/bin/env bash
# Copyright (c) 2026 InfAI (CC SES)
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Runs the connector functions against the real appliance, read-only: which
# resources answer, what they report, which events the connector would emit.
#
# The address of the appliance is asked once and stored in .local/samsung.env.
# The client identity is kept in .local/client-cert and must not be thrown away:
# the appliance authorizes one UUID once, and a new key is a new UUID that the
# appliance does not know. .local/ is in .gitignore; the script refuses to run
# if git would track it.
#
#   scripts/try-samsung.sh            run against the stored address
#   scripts/try-samsung.sh --doors    read only the door state, nothing else
#   scripts/try-samsung.sh --reset    ask for the address again
#   SENERGY_PROFILE=jonah scripts/try-samsung.sh
#                                     also look up the configured device type on
#                                     api.senergy.infai.org via credctl (default jonah,
#                                     set SENERGY_PROFILE= to skip the lookup)
#
# Nothing is written to the appliance. To write one value, run the CLI directly:
#   .local/bin/samsung-cli set set_rapid_fridge On

set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
local_dir="$repo/.local"
env_file="$local_dir/samsung.env"
out_dir="$local_dir/out/$(date +%Y%m%d-%H%M%S)"
cli="$local_dir/bin/samsung-cli"
platform_api="https://api.senergy.infai.org"
profile="${SENERGY_PROFILE-jonah}"

# The device type the connector announces the appliance with; see README.
device_type_id="${DEVICE_TYPE_ID:-urn:infai:ses:device-type:cb2cfd90-1ab8-4a6d-a457-7d84e0d4b1fc}"

cd "$repo"
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

# the client key is the identity towards the appliance; never in a tracked place
if ! git check-ignore -q "$env_file"; then
	echo "$env_file is not ignored by git, aborting" >&2
	exit 1
fi

ask_address() {
	local host port
	read -r -p "appliance address (IP in the local network): " host
	read -r -p "OCF port [49155]: " port
	mkdir -p "$local_dir"
	chmod 700 "$local_dir"
	(
		umask 077
		{
			printf 'OCF_HOST=%q\n' "$host"
			printf 'OCF_PORT=%q\n' "${port:-49155}"
		} > "$env_file"
	)
	echo "stored in $env_file"
}

doors_only=false
reset=false
for arg in "$@"; do
	case "$arg" in
		--doors) doors_only=true ;;
		--reset) reset=true ;;
		*) echo "unknown argument $arg (--doors, --reset)" >&2; exit 2 ;;
	esac
done

if [[ "$reset" == true || ! -f "$env_file" ]]; then
	ask_address
fi
set -a
# shellcheck disable=SC1090
source "$env_file"
# CONNECTOR_ID is required by the shared config loader; the CLI never announces
# anything, so the value only has to exist.
CONNECTOR_ID="${CONNECTOR_ID:-try-samsung-cli}"
# The identity has to survive the run: the appliance authorizes this UUID once.
CERT_PATH="$local_dir/client-cert"
set +a

mkdir -p "$local_dir/bin" "$out_dir"
go build -o "$cli" ./cmd/samsung-cli

section() { printf '\n== %s\n' "$1"; }

# --doors answers one question: what does the appliance report about its doors
# right now. Everything else is left out, including the sections that exist to
# explain a failing run; a door read that fails is read with the full run.
if [[ "$doors_only" == true ]]; then
	section "Doors"
	printf -- '-- %s\n' "/doors/vs/0"
	if "$cli" get /doors/vs/0 > "$out_dir/doors.json" 2> "$out_dir/doors.err"; then
		jq -r '.' "$out_dir/doors.json"
	else
		cat "$out_dir/doors.err" >&2
		exit 1
	fi
	printf -- '\n-- event the connector would emit\n'
	"$cli" -json events doors > "$out_dir/doors-event.json" 2> "$out_dir/doors-event.err" || true
	if [[ -s "$out_dir/doors-event.json" ]]; then
		jq -r '.doors | tojson' "$out_dir/doors-event.json"
	else
		cat "$out_dir/doors-event.err" >&2
	fi
	section "Output"
	echo "$out_dir"
	exit 0
fi

section "Client identity"
"$cli" uuid
"$cli" -json uuid > "$out_dir/uuid.json"
echo
echo "This UUID has to be authorized on the appliance (/oic/sec/acl). Without the"
echo "entry the handshake still succeeds and the public resources below answer,"
echo "but the resources the connector reads run into their timeout: the appliance"
echo "drops a request it does not allow instead of answering 4.03."

section "Device"
if ! "$cli" info; then
	echo
	echo "The appliance did not answer /oic/d. Either it is unreachable at" >&2
	echo "$OCF_HOST:$OCF_PORT, or it does not know this client UUID." >&2
	exit 1
fi
"$cli" -json info > "$out_dir/oic-d.json"

section "Resources the appliance offers"
# /oic/res lists what the appliance exposes. A path listed here that times out
# below is a question of authorization, not of the path being wrong.
if "$cli" resources; then
	"$cli" -json resources > "$out_dir/oic-res.json"
else
	echo "discovery failed; the resources the connector uses are read individually below"
fi

# The ownership state of the appliance. /oic/sec/doxm says whether it is owned
# and by which device, /oic/sec/pstat in which provisioning state it is. Both
# are readable without authorization, so they answer why a read is withheld.
section "Ownership"
for path in /oic/sec/doxm /oic/sec/pstat; do
	printf -- '-- %s\n' "$path"
	name="$(basename "$path")"
	if "$cli" get "$path" > "$out_dir/$name.json" 2> "$out_dir/$name.err"; then
		jq -r '.' "$out_dir/$name.json"
	else
		cat "$out_dir/$name.err"
	fi
done

# One run per section, written as JSON and rendered from the file: a second run
# would pay the timeout of every unreachable resource a second time.
section "Resources the connector reads"
"$cli" -json read > "$out_dir/resources.json" 2> "$out_dir/resources.err" || true
if [[ -s "$out_dir/resources.json" ]]; then
	jq -r 'to_entries[] | "\n-- \(.key)", (.value | tojson)' "$out_dir/resources.json"
else
	cat "$out_dir/resources.err"
fi

section "Events the connector would emit"
"$cli" -json events > "$out_dir/events.json" 2> "$out_dir/events.err" || true
if [[ -s "$out_dir/events.json" ]]; then
	jq -r 'to_entries[] | "\n-- \(.key)", (.value | tojson)' "$out_dir/events.json"
else
	cat "$out_dir/events.err"
fi

# A timeout on a resource that discovery listed means the client is not
# authorized for it. Saying so here keeps the run from reading as "the appliance
# is broken" when the single missing step is the ACL entry.
if [[ -s "$out_dir/resources.json" ]] \
	&& jq -e 'to_entries | map(select(.value.error? // "" | test("deadline exceeded"))) | length > 0' \
		"$out_dir/resources.json" >/dev/null; then
	echo
	echo "Resources that ran into the timeout:"
	jq -r 'to_entries[] | select(.value.error? // "" | test("deadline exceeded")) | "  \(.key)"' \
		"$out_dir/resources.json"
	echo "The appliance answered /oic/d, so it is reachable and the handshake works."
	echo "A request the appliance does not allow is dropped, not refused, so this is"
	echo "what a missing authorization looks like. Add the client UUID above to"
	echo "/oic/sec/acl on the appliance; that needs an identity the appliance already"
	echo "grants full access to."
fi

section "Device type"
if [[ -n "$profile" ]] && command -v credctl >/dev/null; then
	if credctl run "$profile" -- curl -sf "$platform_api/device-repository/v3/device-types/$device_type_id" \
		> "$out_dir/device-type.json" 2> "$out_dir/device-type.err"; then
		jq -r '"\(.name) (\(.id))\nservices:", (.services[] | "  \(.local_id)\n    \(.name)\n    interaction: \(.interaction)")' \
			"$out_dir/device-type.json"
		echo
		echo "service ids of the device type against the ones the connector registers:"
		jq -r '[.services[].local_id] | sort | .[]' "$out_dir/device-type.json" > "$out_dir/dt-services.txt"
		printf '%s\n' temperatures doors energy refrigeration \
			set_fridge_setpoint set_freezer_setpoint set_rapid_fridge set_rapid_freezing \
			| sort > "$out_dir/connector-services.txt"
		if diff -u "$out_dir/dt-services.txt" "$out_dir/connector-services.txt" \
			--label "device type $device_type_id" --label "connector"; then
			echo "  they match"
		fi
	else
		echo "lookup failed: $(cat "$out_dir/device-type.err")"
	fi
else
	echo "no platform lookup (credctl missing or SENERGY_PROFILE empty)"
fi

section "Output"
echo "$out_dir"
echo "Client identity: $CERT_PATH (self-signed; the UUID is OCF_CLIENT_UUID, not the key)"
echo "Nothing was written to the appliance or to the platform."
