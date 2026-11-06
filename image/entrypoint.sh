#!/bin/bash
set -euo pipefail
: "${JITCONFIG:?JITCONFIG env var is required}"

# Docker startup overlaps the runner handshake; Docker jobs must wait for the daemon to be ready.
dockerd >/var/log/dockerd.log 2>&1 &

# Trust the sandbox egress-proxy CA in the runner's Node.js processes.
export NODE_EXTRA_CA_CERTS="${NODE_EXTRA_CA_CERTS:-/etc/ssl/certs/adc-egress-proxy-ca.crt}"
cd /home/runner/actions-runner
exec runuser -u runner -- ./run.sh --jitconfig "$JITCONFIG"
