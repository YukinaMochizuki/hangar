#!/usr/bin/env bash
# Builds what test/testbed/host-bootstrap.sh needs to test the front door
# without a real CA or real DNS: hangar-edge built with the testbed tag, whose
# --acme-dns testbed:<host:port> answers DNS-01 challenges itself, and
# pebble, Let's Encrypt's test CA, with the certificate it serves its
# directory under and the root that certificate chains to.
#
#   front-door-kit.sh OUT
#
# Pass OUT to the test as HANGAR_TESTBED_FRONT_DOOR (docs/runbook/testbed.md).
# Never install this hangar-edge on a real host: it is not a release.
set -euo pipefail

[ $# = 1 ] || { echo "usage: $0 OUT" >&2; exit 2; }
repo=$(cd "$(dirname "$0")/../.." && pwd)
out=$1
mkdir -p "$out"
cd "$repo"
export CGO_ENABLED=0
go build -trimpath -tags testbed -o "$out/hangar-edge" ./cmd/hangar-edge
go build -trimpath -o "$out/pebble" github.com/letsencrypt/pebble/v2/cmd/pebble
certs=$(go list -m -f '{{.Dir}}' github.com/letsencrypt/pebble/v2)/test/certs
install -m 0644 "$certs/localhost/cert.pem" "$out/pebble-cert.pem"
install -m 0600 "$certs/localhost/key.pem" "$out/pebble-key.pem"
install -m 0644 "$certs/pebble.minica.pem" "$out/pebble-ca.pem"
echo "$out"
