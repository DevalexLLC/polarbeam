#!/bin/sh
# Dev-only: self-signed server TLS certificate for the compose stack.
# Production certs are operator-provided (see deploy/compose/server.example.yaml).
set -eu

DIR=/etc/polarbeam/tls
if [ -s "$DIR/server.crt" ] && [ -s "$DIR/server.key" ]; then
    echo "certgen: dev server certificate already present, leaving it alone"
    exit 0
fi

echo "certgen: generating dev server certificate"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$DIR/server.key" -out "$DIR/server.crt" -days 825 \
    -subj "/CN=polarbeam-dev" \
    -addext "subjectAltName=DNS:grpc.polarbeam.local,DNS:polarbeam.local,DNS:localhost,DNS:server,DNS:proxy"
chmod 600 "$DIR/server.key"
# The server runs as uid 10001 (non-root image) and must read the key.
chown 10001 "$DIR/server.key" "$DIR/server.crt"
echo "certgen: wrote $DIR/server.crt"
