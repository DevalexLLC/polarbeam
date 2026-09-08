# PolarBEAM quick start

Get two sites measuring latency and packet loss in both directions using
container images. This walkthrough uses IP addresses and a self-signed
dashboard certificate, so you need no DNS records or existing certificates.

You need two Linux hosts, one at each site, with Docker and access to pull
images. The server can share either host; allow it 2 CPUs, 4 GB RAM, and
20 GB disk. On the server host, also have Docker Compose (with `up --wait`
support), curl, OpenSSL, and sudo. Run Docker commands as a user with Docker
access. Allow TCP 443 to the server from your browser and both agents, and
ICMP between the two site hosts over your existing network or VPN.

## 1. Prepare the server

**On the server host**, choose a release tag and the server's reachable IPv4
address. Start in a fresh directory and keep this shell open for steps 1–2:

```sh
POLARBEAM_VERSION='<version>'
SERVER_IP='<server-ip>'
mkdir polarbeam-install && cd polarbeam-install
```

This downloads the matching Compose file, generates the configuration and a
30-day dashboard certificate, and pulls the server images. It refuses to
overwrite existing configuration. Paste each block as a whole; stop if a
command reports an error.

```sh
(
  set -euC
  umask 077
  curl --fail --location --output docker-compose.yml \
    "https://raw.githubusercontent.com/devalexllc/polarbeam/${POLARBEAM_VERSION}/deploy/compose/docker-compose.yml"

  DB_PASSWORD=$(openssl rand -hex 32)
  cat > .env <<EOF
POLARBEAM_VERSION=${POLARBEAM_VERSION}
POLARBEAM_DB_PASSWORD=${DB_PASSWORD}
POLARBEAM_GRPC_SNI=grpc.polarbeam.local
EOF
  cat > server.yaml <<EOF
listen:
  grpc_hostname: grpc.polarbeam.local
  proxy_protocol: true
db:
  url: postgres://polarbeam:${DB_PASSWORD}@timescaledb:5432/polarbeam
tls:
  cert_file: /etc/polarbeam/tls/server.crt
  key_file: /etc/polarbeam/tls/server.key
EOF
  sudo chown 10001:10001 server.yaml

  openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
    -subj "/CN=${SERVER_IP}" -addext "subjectAltName=IP:${SERVER_IP}" \
    -keyout server.key -out server.crt

  docker compose config --quiet
  docker compose pull
  docker compose run --rm --no-deps --user 0 --entrypoint sh \
    --volume "$PWD/server.crt:/in/server.crt:ro" \
    --volume "$PWD/server.key:/in/server.key:ro" \
    server -ec '
      cp /in/server.crt /etc/polarbeam/tls/server.crt
      cp /in/server.key /etc/polarbeam/tls/server.key
      chown 10001:10001 /etc/polarbeam/tls/server.crt /etc/polarbeam/tls/server.key
      chmod 0644 /etc/polarbeam/tls/server.crt
      chmod 0600 /etc/polarbeam/tls/server.key
    '
)
```

## 2. Start PolarBEAM and prepare both sites

**In the same server shell**, initialize the database and agent CA, start
PolarBEAM, and create your dashboard login. The last command prompts twice for an
administrator password of at least eight characters.

```sh
(
  set -e
  docker compose up -d --wait timescaledb
  docker compose run --rm server migrate
  docker compose run --rm server ca init --if-missing
  docker compose up -d --wait
  docker compose exec server polarbeam-server user add --username admin --admin
)
```

Create a token for each site and configure their shared ICMP mesh. **Save each
site's token and the `sha256:…` CA fingerprint** printed with it for step 3.
Each token can enroll one agent and expires after 24 hours.

```sh
(
  set -e
  docker compose exec server polarbeam-server token create --site site-a
  docker compose exec server polarbeam-server token create --site site-b
  docker compose exec server polarbeam-server mesh create --name wan
  docker compose exec server polarbeam-server mesh add --name wan --site site-a
  docker compose exec server polarbeam-server mesh add --name wan --site site-b
  docker compose exec server polarbeam-server probe add --mesh wan --type icmp
)
```

Both sites use the built-in `default` network. The probe sends 10 pings every
30 seconds, with 200 ms between packets and a 5-second timeout.

## 3. Start an agent at each site

**Run this step on each site host.** Use the same release, server IP, and CA
fingerprint on both hosts, with the matching token and probe address:

| Host | `JOIN_TOKEN` | `PROBE_IP` |
|---|---|---|
| Site A | Token printed for `site-a` | Site A's IP reachable from Site B |
| Site B | Token printed for `site-b` | Site B's IP reachable from Site A |

The probe IP is the site host's reachable address, not a container address.

```sh
POLARBEAM_VERSION='<version>'
SERVER_IP='<server-ip>'
JOIN_TOKEN='<this-sites-token>'
CA_FINGERPRINT='sha256:<fingerprint>'
PROBE_IP='<this-sites-probe-ip>'

(
  set -eu
  mkdir -p polarbeam-agent
  cd polarbeam-agent
  cat > agent.yaml <<EOF
server:
  address: ${SERVER_IP}:443
  sni: grpc.polarbeam.local
EOF
  chmod 0644 agent.yaml

  docker run --rm \
    --cap-add NET_RAW \
    --mount "type=bind,src=$PWD/agent.yaml,dst=/etc/polarbeam/agent.yaml,readonly" \
    --mount type=volume,src=polarbeam-agent-state,dst=/var/lib/polarbeam-agent \
    "ghcr.io/devalexllc/polarbeam-agent:${POLARBEAM_VERSION}" \
    enroll --token "$JOIN_TOKEN" --fingerprint "$CA_FINGERPRINT" \
    --probe-address "$PROBE_IP"

  docker run -d \
    --name polarbeam-agent \
    --restart unless-stopped \
    --cap-add NET_RAW \
    --mount "type=bind,src=$PWD/agent.yaml,dst=/etc/polarbeam/agent.yaml,readonly" \
    --mount type=volume,src=polarbeam-agent-state,dst=/var/lib/polarbeam-agent \
    "ghcr.io/devalexllc/polarbeam-agent:${POLARBEAM_VERSION}"
)
```

If enrollment fails, correct the inputs and repeat this block. An expired
token or one used by another agent needs a fresh token from step 2.

Docker pulls the agent image on first use. Each host's named volume preserves
its agent identity and buffered results. `grpc.polarbeam.local` is the TLS
routing name; agents connect to the server IP, so it needs no DNS record.
The CA fingerprint authenticates enrollment independently of the dashboard
certificate.

## 4. View your measurements

Open **`https://<server-ip>/`** and sign in as **admin**. Your browser will
warn about the self-signed certificate; accept the exception for this trial
installation. The certificate expires after 30 days.

On **Agents**, confirm both agents have recent updates. After roughly a
minute, open **Overview → Matrix** to see latency and packet loss for
**site-a → site-b** and **site-b → site-a**. Click either direction to explore
its measurements.

For trusted HTTPS, production setup, more probe types, or troubleshooting,
continue with the [complete installation and user guide](install.md).
