#!/bin/sh
# Dev agent entrypoint: wait for this site's join token, enroll once, run.
set -eu

SITE="${POLARBEAM_SITE:?POLARBEAM_SITE must be set}"
CONFIG=/etc/polarbeam/agent.yaml
PKI=/var/lib/polarbeam-agent/pki

if [ ! -s "$PKI/agent.crt" ]; then
    TOKEN_FILE="/bootstrap/${SITE}.token"
    echo "agent[$SITE]: waiting for join token $TOKEN_FILE"
    while [ ! -s "$TOKEN_FILE" ]; do sleep 2; done
    echo "agent[$SITE]: enrolling"
    # --probe-address is the compose service name: behind the SNI proxy the
    # server observes the proxy's source address, not the agent's.
    polarbeam-agent enroll --config "$CONFIG" \
        --token "$(cat "$TOKEN_FILE")" --ca-cert /bootstrap/ca.crt \
        --probe-address "agent-${SITE}"
fi

exec polarbeam-agent run --config "$CONFIG"
