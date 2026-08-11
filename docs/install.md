# PolarBEAM installation and user guide

This guide starts with an empty environment and ends with a production
control plane, enrolled agents, a two-site probe mesh, and measurements in the
dashboard. It covers online image pulls, offline release bundles, and
container agents.

The control plane and the agents are all deployed as containers; the agent
container runs on any host with Docker or Podman.

## 1. Plan the installation

Choose these values before changing any configuration. The examples below use
the values in the right-hand column; replace them everywhere with the values
for your environment.

| Value | Purpose | Example |
|---|---|---|
| `<version>` | PolarBEAM release/image tag | `v1.0.0` |
| `<arch>` | Control-plane CPU architecture | `amd64` or `arm64` |
| `<control-plane-ip>` | Address of the Docker host | `192.0.2.10` |
| `<dashboard-name>` | Browser-facing DNS name | `polarbeam.example.com` |
| `<grpc-name>` | Agent-facing DNS name and TLS SNI | `grpc.polarbeam.example.com` |
| `<site-name>` | Stable, short site identifier | `nyc`, `lon` |
| `<probe-address>` | Address other agents can probe | `10.20.0.15` |

Both DNS names must resolve to the control-plane host. They intentionally use
the same IP and TCP port 443. The proxy reads the TLS SNI and sends
`<grpc-name>` to the agent gRPC listener; every other SNI goes to the
dashboard. TLS is not terminated at the proxy.

Use stable site names because they identify sites in mesh and probe
assignments. Each agent also needs a stable probe address reachable from the
other agent sites. This may be a private WAN address, VPN address, or resolvable
hostname. Do not use `localhost`, a container-only address, or a NAT address
that peers cannot reach.

For a useful first deployment, prepare at least two agent systems in different
sites. A single agent can monitor external targets, but directional site-to-site
measurements require two sites.

### Host prerequisites

The control-plane host needs:

- Linux with Docker Engine and the Docker Compose plugin
- persistent storage for Docker volumes
- inbound TCP 443 from operators and every agent
- DNS records for `<dashboard-name>` and `<grpc-name>`
- an HTTPS certificate and private key valid for `<dashboard-name>`

Confirm the container tools before continuing:

```sh
docker version
docker compose version
```

The dashboard certificate may come from a public CA or your organization's
internal CA. Browser clients must trust its issuer. Do not use this certificate
for agent identity: PolarBEAM creates and manages a separate built-in CA for
agent mTLS and the gRPC server certificate.

## 2. Obtain the release files and images

Create a dedicated installation directory on the control-plane host. All
remaining control-plane commands in this guide run from that directory.

The directory must contain:

```text
docker-compose.yml
env.example
server.example.yaml
```

These files are in every release bundle. They are also available under
`deploy/compose/` in the source repository.

### Online installation

Check out the matching release tag, copy its production deployment files into
a clean install directory, and enter that directory:

```sh
git clone --depth 1 --branch <version> \
  https://github.com/devalexllc/polarbeam.git polarbeam-source
mkdir polarbeam-install
cp polarbeam-source/deploy/compose/docker-compose.yml polarbeam-install/
cp polarbeam-source/deploy/compose/env.example polarbeam-install/
cp polarbeam-source/deploy/compose/server.example.yaml polarbeam-install/
cd polarbeam-install
```

Alternatively, extract the matching release bundle and use its directory even
on an online host. Then pull all four runtime images. Pin a release tag; do not
use `latest` for a production installation.

```sh
export POLARBEAM_VERSION=<version>

docker pull ghcr.io/devalexllc/polarbeam-server:${POLARBEAM_VERSION}
docker pull ghcr.io/devalexllc/polarbeam-proxy:${POLARBEAM_VERSION}
docker pull ghcr.io/devalexllc/polarbeam-agent:${POLARBEAM_VERSION}
docker pull timescale/timescaledb-ha:pg16-all
```

The agent image is pulled here even though it is not a control-plane Compose
service. Container-based agent systems may pull it directly instead.

### Offline installation

The release bundle contains only PolarBEAM's own artifacts — third-party
images are never redistributed. Two transfers are therefore required: the
bundle itself, and the TimescaleDB image.

On an internet-connected transfer system, download the release artifact named
`polarbeam-<version>-<arch>-bundle.tar.gz` and extract it there first — the
bundle's `TIMESCALEDB-IMAGE` file names the digest-qualified TimescaleDB
reference this release was assembled against (the rolling tag alone would not
identify exact bytes). Pull by that digest, re-apply the tag the Compose file
uses, and save the image:

```sh
tar xzf polarbeam-<version>-<arch>-bundle.tar.gz
docker pull --platform linux/<arch> \
  "$(cat polarbeam-<version>-<arch>-bundle/TIMESCALEDB-IMAGE)"
docker tag "$(cat polarbeam-<version>-<arch>-bundle/TIMESCALEDB-IMAGE)" \
  timescale/timescaledb-ha:pg16-all
docker save -o timescaledb-<arch>.tar timescale/timescaledb-ha:pg16-all
```

The tag step matters: a by-digest pull leaves the image untagged, and the
Compose file references the tag — saving the tagged image means the load on
the offline host restores a tag that points at exactly the pinned bytes. The
digest changes rarely, so upgrades usually reuse the copy you already
transferred (compare `TIMESCALEDB-IMAGE` across bundles).

Transfer both files to the control-plane host, then extract and verify the
bundle and load all images:

```sh
tar xzf polarbeam-<version>-<arch>-bundle.tar.gz
cd polarbeam-<version>-<arch>-bundle
sha256sum -c SHA256SUMS

# PolarBEAM server, proxy, and agent images from the bundle:
docker load -i images/polarbeam-images-<version>-<arch>.tar
# The separately transferred database image:
docker load -i ../timescaledb-<arch>.tar
```

For an offline agent host, save just the already-loaded agent image, transfer
the result, and load it on the agent system:

```sh
# On the control-plane or transfer host after the bundle's docker load:
docker save -o polarbeam-agent-<version>-<arch>.tar \
  ghcr.io/devalexllc/polarbeam-agent:<version>

# On the offline agent host:
docker load -i polarbeam-agent-<version>-<arch>.tar
```

## 3. Configure DNS, TLS, and the firewall

Create these DNS records before starting PolarBEAM:

```text
<dashboard-name>  A/AAAA  <control-plane-ip>
<grpc-name>       A/AAAA  <control-plane-ip>
```

The dashboard certificate file should contain the full certificate chain and
must contain `<dashboard-name>` in its Subject Alternative Name. PolarBEAM
auto-issues the gRPC listener certificate with `<grpc-name>` as its SAN, so do
not add the gRPC name to the dashboard certificate unless it is also useful
for your environment.

Open inbound TCP 443 on the control-plane host. Do not expose the server's
ports 8080 or 8443 or TimescaleDB port 5432; those stay on the private Compose
network.

Agent and probe firewall requirements are listed in
[Firewall requirements](#firewall-requirements). Review them before enrolling
agents, especially when sites are connected through restrictive WAN
firewalls.

## 4. Configure the proxy and control plane

### 4.1 Create `.env`

Copy the environment example:

```sh
cp env.example .env
chmod 600 .env
```

Generate a long database password. A hexadecimal value avoids URL-encoding
problems when the same password is placed in `server.yaml`:

```sh
openssl rand -hex 32
```

Edit `.env` and set all three values:

```dotenv
POLARBEAM_DB_PASSWORD=<generated-hex-password>
POLARBEAM_VERSION=<version>
POLARBEAM_GRPC_SNI=<grpc-name>
```

`POLARBEAM_GRPC_SNI` configures the proxy. It must exactly match the gRPC DNS
name agents send as TLS SNI. The supplied nginx proxy performs TCP passthrough
with this routing rule:

```text
SNI = <grpc-name>  -> server:8443  (agent gRPC and mTLS)
all other SNI      -> server:8080  (dashboard HTTPS)
```

On both routes the proxy prepends a PROXY protocol v1 header carrying the
real client address, and the server requires it (`listen.proxy_protocol:
true` in `server.yaml`). This is what keys login rate limiting per client
instead of per proxy. If you replace the bundled proxy, the replacement must
also send PROXY protocol to both backends (haproxy: `send-proxy` /
`send-proxy-v2`; traefik: `proxyProtocol` on the TCP service) — or set
`listen.proxy_protocol: false` and accept that every dashboard login then
shares one rate-limit bucket.

### 4.2 Create `server.yaml`

Copy and protect the server example. The file is bind-mounted into the
server container, which runs as UID 10001, so the container must be able to
read it — owner-only permissions with the operator's ownership would break
migration, CA initialization, and startup:

```sh
cp server.example.yaml server.yaml
sudo chown 10001 server.yaml
chmod 600 server.yaml
```

Edit `server.yaml`. A minimal production configuration is:

```yaml
listen:
  grpc: ':8443'
  grpc_hostname: <grpc-name>
  http: ':8080'
  proxy_protocol: true

db:
  url: postgres://polarbeam:<generated-hex-password>@timescaledb:5432/polarbeam

tls:
  cert_file: /etc/polarbeam/tls/server.crt
  key_file: /etc/polarbeam/tls/server.key

ca:
  dir: /var/lib/polarbeam-server/ca

log:
  level: info
```

The value of `listen.grpc_hostname` must exactly equal
`POLARBEAM_GRPC_SNI`. It is also the SAN on the automatically issued gRPC
server certificate.

Configuration loading is strict. Unknown or misspelled YAML keys stop the
server, so copy keys from the supplied example instead of guessing them. The
default agent certificate lifetime is 30 days and the default gRPC server
certificate lifetime is 90 days; both rotate automatically.

### 4.3 Install the dashboard certificate

The server needs two PEM files: a certificate file whose first block is the
leaf, followed by any intermediates, and an **unencrypted** private key. The
key is protected by file mode, not by a passphrase — the server has no way to
prompt for one, so an encrypted key stops startup.

If your CA issued a PKCS#12 bundle (`.p12` or `.pfx`) rather than PEM files,
convert it first. Each command prompts for the bundle's import password:

```sh
(
  set -e
  umask 077   # the extracted key is unencrypted; do not create it world-readable
  trap 'rm -f leaf.tmp chain.tmp key.tmp cert.new key.new' EXIT
  rm -f leaf.tmp chain.tmp key.tmp cert.new key.new

  openssl pkcs12 -in dashboard.p12 -clcerts -nokeys -out leaf.tmp
  openssl pkcs12 -in dashboard.p12 -cacerts -nokeys -out chain.tmp
  openssl pkcs12 -in dashboard.p12 -nocerts -nodes  -out key.tmp

  test "$(grep -c -- '-----BEGIN' key.tmp)" = 1
  test "$(grep -c -- '-----BEGIN CERTIFICATE' leaf.tmp)" = 1

  pem="/BEGIN CERTIFICATE/,/END CERTIFICATE/p"
  sed -n "$pem" leaf.tmp  >  cert.new
  sed -n "$pem" chain.tmp >> cert.new
  openssl pkey -in key.tmp -out key.new
  chmod 600 key.new

  mv -T cert.new dashboard-cert.pem
  mv -T key.new  dashboard-key.pem
)
```

Every piece of that block earns its place, because each failure it prevents is
silent:

- The `( set -e … )` subshell stops at the first failing command instead of
  carrying on with whatever the failure left behind. It has to be a subshell:
  a bare `set -e` pasted into an interactive shell would close the session on
  the first error. Run it on its own line, too — putting it inside `if` or on
  either side of `&&` makes the shell treat its status as tested, which
  disables `set -e` for everything inside it and quietly removes every
  protection below.
- Extraction writes to `-out` files rather than piping into `sed`, because a
  pipeline reports the exit status of `sed`, which succeeds on empty input. A
  wrong password would otherwise leave an empty `dashboard-cert.pem` behind a
  reported success.
- The two `test` lines reject multi-alias keystores. A PKCS#12 file may hold
  several identities; `openssl pkey` then writes only the first key it finds
  and exits 0, so the dashboard would silently serve whichever identity
  happened to be stored first. If either check fails, re-export the bundle
  for the one alias you want rather than editing the files by hand.
- The `trap … EXIT` deletes `key.tmp` on every path out of the subshell. It
  holds the private key in plaintext, and the failures above are ones this
  block expects to hit — a bare cleanup line at the end would be skipped by
  `set -e` precisely when a conversion aborted midway, stranding the key.
- Everything is built under temporary names and moved into place only after
  the last check passes, so a rerun that fails — mistyped password, missing
  bundle, multi-alias keystore — leaves the certificate and key you already
  had untouched. Writing straight to `dashboard-key.pem` would destroy
  working key material on exactly the attempts this block is designed to
  catch, and the `trap` clears the half-built files on the way out.
- `umask` plus the `chmod` protect the key, and the rename is what makes them
  stick. `umask` applies only when a file is created, so writing over an
  existing `0644` `dashboard-key.pem` would truncate it and keep that mode —
  publishing an unencrypted private key to every account on the host. A
  rename replaces the destination outright, so the file that lands is the
  `0600` one just created.
- `mv -T` treats each destination as a file, never as a directory to move
  into. This is not hypothetical here: Docker creates a directory at any
  bind-mount source path that does not exist, so running the install command
  below before converting leaves directories named `dashboard-cert.pem` and
  `dashboard-key.pem`. Plain `mv` would drop the new files inside them and
  exit 0, reporting success while the paths the server reads stay wrong.

Extract the leaf (`-clcerts`) and the intermediates (`-cacerts`) as separate
steps, in that order: the server treats the first certificate in the file as
the leaf and requires it to match the key, and `-nokeys` on its own emits
every certificate in the bundle in no guaranteed order. Dropping the
intermediates leaves browsers that do not already hold them unable to build a
chain.

The `sed` filters strip the `Bag Attributes` preambles that PKCS#12
extraction emits, and are safe in a pipeline because their input is a local
file that already exists. Do not substitute `openssl x509` for them: it reads
a single certificate, so a bundle carrying two or more intermediates would be
silently truncated to the first one, producing a chain that fails to validate
only for the clients that needed the discarded issuer. `openssl pkey`
normalizes the key to PKCS#8, and the single-key check above is what makes
that safe.

If either `openssl pkcs12` command fails with an unsupported-algorithm or
`digital envelope routines` error, the bundle uses legacy RC2/40-bit crypto
that OpenSSL 3 disables by default; add `-legacy` to that command.

Verify the result before installing it. The SAN must cover the hostname
operators browse to — a name mismatch is a browser warning rather than a
server error, so it is easy to ship unnoticed:

```sh
(
  set -e
  openssl x509 -in dashboard-cert.pem -noout -subject -dates -ext subjectAltName
  openssl crl2pkcs7 -nocrl -certfile dashboard-cert.pem | openssl pkcs7 -print_certs -noout

  openssl x509 -in dashboard-cert.pem -noout -pubkey > cert.pub
  openssl pkey -in dashboard-key.pem -pubout        > key.pub
  test -s cert.pub
  test -s key.pub
  cmp cert.pub key.pub
  rm -f cert.pub key.pub
  echo "key matches certificate"
)
```

`key matches certificate` prints only when every check above it passed; treat
its absence as failure regardless of what else scrolled past. Two details of
that block are load-bearing:

- The un-piped `openssl x509` runs first, so a malformed certificate file is
  caught before the piped `crl2pkcs7` command, whose exit status would come
  from `pkcs7` at the end of the pipe.
- The `test -s` checks are separate commands rather than an
  `A && B && cmp` chain. `set -e` deliberately ignores a failure in every
  part of an `&&` list but the last, so chained guards would let two empty
  files fall through to the success message — the digest-comparison trap in a
  different disguise. As written, an empty file stops the subshell.

The last command prints the certificates in the order the server will send
them. A correct file reads as one ladder: the leaf first, then each
certificate's `issuer` matching the next one's `subject`.

Repair the file by hand if it does not. PKCS#12 stores CA certificates in no
particular order and `-cacerts` emits them exactly as stored, so a bundle
whose bags were written root-first — or that carries CA certificates
belonging to some other chain — produces a broken ladder even though every
command above succeeded. The server transmits the chain in file order and
never reorders it, so clients that cannot repair the order themselves reject
the connection. The file is only concatenated PEM blocks: move them into
issuer order in a text editor, and delete any certificate that is not part of
this leaf's ladder.

Sending the root as well is merely wasteful — clients must already trust it —
so removing it is an optimization, not a requirement. Do not treat matching
`issuer` and `subject` as proof that a certificate is the root: during a CA
key rollover an intermediate can be self-*issued* while still being signed by
the previous key, and deleting it breaks the chain for every client that
needed it. When the two are hard to tell apart, keep both. A redundant
certificate costs a few hundred bytes per handshake; a missing one costs
availability.

The server container runs as UID 10001. Copy the operator-managed dashboard
certificate and key into the Compose `tls` volume and set readable ownership.
Replace the two `/absolute/path/...` values:

```sh
docker compose run --rm --no-deps \
  -v /absolute/path/dashboard-cert.pem:/in/cert.pem:ro \
  -v /absolute/path/dashboard-key.pem:/in/key.pem:ro \
  --entrypoint sh --user 0 server -c \
  'cp /in/cert.pem /etc/polarbeam/tls/server.crt && \
   cp /in/key.pem /etc/polarbeam/tls/server.key && \
   chown 10001:10001 /etc/polarbeam/tls/server.crt /etc/polarbeam/tls/server.key && \
   chmod 0644 /etc/polarbeam/tls/server.crt && \
   chmod 0600 /etc/polarbeam/tls/server.key'
```

On an SELinux-enforcing host, add the appropriate bind-mount relabel option
for the two source files if Docker cannot read them.

The certificate and key now live in the `tls` volume. If you converted from a
PKCS#12 bundle, the working directory still holds an unencrypted private key —
move it to wherever you keep key material, or delete it along with the bundle.

## 5. Initialize and deploy the control plane

Validate that Compose can resolve the configuration without printing the
expanded file:

```sh
docker compose config --quiet
```

Start TimescaleDB and wait until it reports healthy:

```sh
docker compose up -d timescaledb
docker compose ps
```

Apply the database migrations. Production never auto-migrates:

```sh
docker compose run --rm server migrate \
  --config /etc/polarbeam/server.yaml
```

Create the built-in PolarBEAM CA. Run this once on a new installation. The
command refuses to overwrite an existing CA:

```sh
docker compose run --rm server ca init \
  --config /etc/polarbeam/server.yaml
```

Start the complete control plane:

```sh
docker compose up -d
docker compose ps
```

Inspect startup logs. Resolve every error before enrolling agents:

```sh
docker compose logs --tail=100 timescaledb server proxy
```

Create the first dashboard administrator. The command prompts twice for a
password of at least eight characters:

```sh
docker compose exec server polarbeam-server user add \
  --config /etc/polarbeam/server.yaml \
  --username admin --admin
```

Open `https://<dashboard-name>/` and sign in. A successful health request also
confirms that DNS, the proxy's default route, dashboard TLS, and the HTTP
listener work together:

```sh
curl https://<dashboard-name>/healthz
```

If DNS is not live yet, test against the control-plane IP while preserving
the TLS SNI:

```sh
curl --resolve <dashboard-name>:443:<control-plane-ip> \
  https://<dashboard-name>/healthz
```

Do not use `curl -k` as the final verification; it hides certificate trust and
hostname errors that browsers will also encounter.

## 6. Understand agent configuration

Agent YAML configures only the local agent and its control-plane connection.
Probe targets, mesh membership, intervals, and timeouts are configured
centrally after enrollment and streamed to agents. Do not put probe definitions
in `agent.yaml`.

Use this configuration on every agent, changing only values that differ for
that host:

```yaml
server:
  address: <grpc-name>:443
  sni: <grpc-name>

state_dir: /var/lib/polarbeam-agent

spool:
  max_bytes: 268435456
  max_age: 168h

log:
  level: info
```

The fields mean:

- `server.address`: the reachable proxy endpoint in `host:port` form. Normally
  this is `<grpc-name>:443`.
- `server.sni`: must equal the control plane's `listen.grpc_hostname` and
  `POLARBEAM_GRPC_SNI`.
- `state_dir`: persistent PKI identity and offline result spool. Do not share
  one state directory or volume between agents.
- `spool.max_bytes`: maximum local spool size; the oldest results are dropped
  first when full, and the loss is reported to the control plane.
- `spool.max_age`: maximum age of disconnected results before dropping them.
- `log.level`: `debug`, `info`, `warn`, or `error`.

The enrollment command also takes `--probe-address`. This is not the control
plane address. It is the IP address or DNS name other agents should probe when
this agent participates in a mesh. Always supply it: without it the control
plane falls back to the observed connection source — with the bundled proxy's
PROXY protocol that is the agent's real egress address rather than the proxy,
but an egress address is still usually a NAT boundary, not the address other
sites can probe.

## 7. Create an enrollment token

Create one single-use token for each agent. The site is created automatically
the first time its name is used:

```sh
docker compose exec server polarbeam-server token create \
  --config /etc/polarbeam/server.yaml \
  --site <site-name> --ttl 24h
```

An administrator can also issue tokens from the dashboard under
**user menu -> Settings -> Enrollment**, after creating the site on the
**Sites** tab (the dashboard never auto-creates sites — only the CLI command
above does). The token value is displayed exactly once in either case. The
Enrollment tab also lists every token with its status and lets an
administrator delete an unused token, which revokes it immediately; used
tokens are kept as the enrollment audit record.

Save both values printed by the command:

- the join token, which is valid until its TTL expires and can be used once
- the `sha256:<hex>` built-in CA fingerprint

Transfer them to the intended agent securely. If a site has multiple agents,
create a separate token for each agent using the same site name.

Enrollment deliberately has no trust-on-first-use mode. The examples below pin
the printed fingerprint. As an alternative, export the public CA certificate,
transfer it to the agent, and use `--ca-cert <file>` instead of
`--fingerprint`:

```sh
docker compose cp \
  server:/var/lib/polarbeam-server/ca/ca.crt ./polarbeam-ca.crt
```

## 8. Deploy a container agent

Use one persistent named volume per agent. The example below enrolls first and
then creates the long-running container, avoiding a restart loop from an
unenrolled agent.

### 8.1 Create the configuration and state volume

```sh
sudo install -d -m 0755 /opt/polarbeam-agent
sudo vi /opt/polarbeam-agent/agent.yaml
sudo chmod 0644 /opt/polarbeam-agent/agent.yaml
docker volume create polarbeam-agent-state
```

Put the configuration from [Understand agent
configuration](#6-understand-agent-configuration) in `agent.yaml`.

If this host is offline, load the agent image tar transferred from the release
bundle. If it is online and the image has not already been pulled, run:

```sh
docker pull ghcr.io/devalexllc/polarbeam-agent:<version>
```

### 8.2 Enroll into the persistent volume

```sh
docker run --rm \
  --cap-add NET_RAW \
  --mount type=bind,src=/opt/polarbeam-agent/agent.yaml,dst=/etc/polarbeam/agent.yaml,readonly \
  --mount type=volume,src=polarbeam-agent-state,dst=/var/lib/polarbeam-agent \
  ghcr.io/devalexllc/polarbeam-agent:<version> \
  enroll --config /etc/polarbeam/agent.yaml \
  --token '<join-token>' \
  --fingerprint 'sha256:<hex>' \
  --probe-address '<probe-address>'
```

`--cap-add NET_RAW` is required even though enrollment sends no ICMP. The
image's binary carries the `cap_net_raw+ep` file capability, and the kernel
refuses to `execve` such a binary when that capability is outside the
container's bounding set — the container dies before the program starts, with:

```text
exec container process `/usr/local/bin/polarbeam-agent`: Operation not permitted
```

Docker includes `NET_RAW` in its default set, so this bites on runtimes that
do not: Podman, rootless daemons, and daemons whose `default-capabilities`
have been narrowed. Every invocation of this image needs the flag, including
one-shot `enroll`, `selfcheck`, and `version` runs.

On an SELinux-enforcing host, add the appropriate relabel option to the config
bind mount if Docker cannot read it.

### 8.3 Start and verify the agent

```sh
docker run -d \
  --name polarbeam-agent \
  --restart unless-stopped \
  --cap-add NET_RAW \
  --mount type=bind,src=/opt/polarbeam-agent/agent.yaml,dst=/etc/polarbeam/agent.yaml,readonly \
  --mount type=volume,src=polarbeam-agent-state,dst=/var/lib/polarbeam-agent \
  ghcr.io/devalexllc/polarbeam-agent:<version>

docker exec polarbeam-agent polarbeam-agent selfcheck \
  --config /etc/polarbeam/agent.yaml
docker logs --tail=100 polarbeam-agent
```

`NET_RAW` enables ICMP and traceroute. TCP, TLS, HTTP, and DNS probes do not
need it, but the recommended standard agent includes it so all supported probe
types work.

## 9. Enroll the remaining sites

Repeat the token, configuration, enrollment, and service/container steps for
every agent. Use a unique persistent state directory or volume and the correct
probe address on each host.

On the control plane, confirm the server sees the connections:

```sh
docker compose logs --tail=100 server
```

The logs should contain `agent enrolled`, `agent connected`, and
`config snapshot sent`. The dashboard's **Agents** page should show every
agent with a recent last-update time.

Optional map metadata can be added after a site's first token creates it:

```sh
docker compose exec server polarbeam-server site set \
  --config /etc/polarbeam/server.yaml \
  --name nyc --display-name 'New York' --location 'New York, US' \
  --lat 40.7128 --lon -74.0060
```

Coordinates must be supplied together. Repeat for each site so it appears on
the dashboard map. `site set --clear-coords` removes a site from the map
again. The same metadata can be edited in the dashboard under
**user menu -> Settings -> Sites**, which also creates sites explicitly and
deletes sites nothing references (clearing both coordinate fields unplaces
the site). Site names are permanent — delete and re-create a site to rename
it.

## 10. Configure probe workloads

An administrator can configure targets, meshes, and probes in the dashboard
from **user menu -> Settings**. The **Targets**, **Meshes**, and **Probes** tabs
provide the same validation as the server CLI. Changes normally reach
connected agents within about 30 seconds; agent YAML edits and restarts are not
required.

The CLI examples below create a minimal working two-site mesh. Replace `nyc`
and `lon` with the exact site names used when their tokens were created.

### 10.1 Create a full mesh

```sh
docker compose exec server polarbeam-server mesh create \
  --config /etc/polarbeam/server.yaml --name wan

docker compose exec server polarbeam-server mesh add \
  --config /etc/polarbeam/server.yaml --name wan --site nyc

docker compose exec server polarbeam-server mesh add \
  --config /etc/polarbeam/server.yaml --name wan --site lon

docker compose exec server polarbeam-server mesh list \
  --config /etc/polarbeam/server.yaml
```

A mesh expands into ordered directions. With two sites, PolarBEAM creates
both `nyc -> lon` and `lon -> nyc` assignments using the probe addresses
recorded during enrollment.

### 10.2 Add baseline ICMP and traceroute probes

```sh
docker compose exec server polarbeam-server probe add \
  --config /etc/polarbeam/server.yaml \
  --mesh wan --type icmp --interval 30s --timeout 5s \
  --train-count 10 --train-spacing 200ms

docker compose exec server polarbeam-server probe add \
  --config /etc/polarbeam/server.yaml \
  --mesh wan --type traceroute --interval 5m --timeout 30s

docker compose exec server polarbeam-server probe list \
  --config /etc/polarbeam/server.yaml
```

The ICMP train must fit inside the timeout, and every timeout must be shorter
than its interval. The CLI and dashboard reject invalid combinations.

TCP and TLS mesh probes require a real service listening at the same port on
each peer probe address. The port belongs on the mesh probe as a parameter:

```sh
docker compose exec server polarbeam-server probe add \
  --config /etc/polarbeam/server.yaml \
  --mesh wan --type tcp --interval 30s --timeout 5s --param port=443
```

Do not add that example unless port 443 is actually reachable on every mesh
member. The PolarBEAM agent itself does not open a probe-listener port.

### 10.3 Add external targets and direct probes

External targets let one site monitor infrastructure that is not a PolarBEAM
agent. Create a target with the fields required by the probe type:

```sh
# Address target for ICMP, traceroute, TCP, TLS, or DNS.
docker compose exec server polarbeam-server target add \
  --config /etc/polarbeam/server.yaml \
  --name public-dns --address 203.0.113.53 --port 53

# URL target for HTTP(S).
docker compose exec server polarbeam-server target add \
  --config /etc/polarbeam/server.yaml \
  --name status-page --url https://status.example.com/health
```

Assign direct probes to the site whose agents should execute them:

```sh
docker compose exec server polarbeam-server probe add \
  --config /etc/polarbeam/server.yaml \
  --site nyc --target public-dns --type dns \
  --interval 30s --timeout 5s \
  --param dns.qname=example.com --param dns.qtype=A

docker compose exec server polarbeam-server probe add \
  --config /etc/polarbeam/server.yaml \
  --site nyc --target status-page --type http \
  --interval 30s --timeout 10s \
  --param http.method=GET --param http.expect_status=2xx
```

Probe-specific rules include:

| Type | Target/parameters |
|---|---|
| `icmp` | target address; optional train count and spacing |
| `traceroute` | target address; agent needs `NET_RAW` |
| `tcp` | direct target address and port, or mesh `port` parameter |
| `tls` | TCP requirements; optional `tls.sni` and `tls.insecure_skip_verify` |
| `http` | direct URL target only; optional method, expected status, and TLS verification override |
| `dns` | target address and required `dns.qname`; optional qtype, expected RCODE, and resolver override |

Avoid `*.insecure_skip_verify=true` except for intentionally self-signed test
services. Unknown parameters are rejected rather than silently ignored.

## 11. Verify the complete system

Wait at least one probe interval plus the roughly 30-second configuration
distribution interval, then verify each layer:

1. `docker compose ps` shows TimescaleDB healthy and the control-plane
   containers running.
2. `https://<dashboard-name>/healthz` succeeds with normal TLS validation.
3. The dashboard **Agents** page shows every agent recently connected.
4. `polarbeam-agent selfcheck` passes fatal checks on every host.
5. `probe list` shows the enabled mesh and direct probes you created.
6. The dashboard **Overview** map or matrix shows both directions between
   mesh sites.
7. The **Routes** page begins to populate after traceroute runs.

Useful logs while verifying are:

```sh
# Control plane
docker compose logs --tail=200 proxy server timescaledb

# Agent
docker logs --tail=200 polarbeam-agent
```

The proxy access log includes the received SNI and selected backend. Agent
connections must show `sni="<grpc-name>"` and `backend=server_grpc`. Browser
connections should use `backend=server_dashboard`.

## Optional: single sign-on (OIDC)

Dashboard sign-in can delegate to an OpenID Connect identity provider such
as Keycloak. SSO is strictly optional and default-off. Local accounts —
including the first admin created with `user add --admin` — keep working
regardless of the OIDC configuration or the provider's availability, so a
local admin is always your break-glass access. Configure at least one local
admin before enabling SSO.

The SSO schema (`users.auth_source`, `users.oidc_subject`, the
`oidc_settings` table) ships with the base schema, so a fresh install
needs no extra steps. If you are upgrading a control plane installed from
v0.1.0, use the standard procedure under [Upgrades](#upgrades) — new
images, then `migrate`, then recreate the services. The upgrade includes
migration `0005`, which runs in a single transaction and scopes federated
identities to their provider: it adds `users.oidc_issuer`, attributes
existing federated accounts to the currently configured issuer, replaces
the unique index on the bare subject with one on `(issuer, subject)`, and
signs out every SSO session once (subjects are only unique within an
issuer, so pre-upgrade sessions cannot be attributed with certainty).
Local accounts and their sessions are untouched; SSO users simply sign in
again.

SSO is the one feature that makes the server perform outbound HTTP:
discovery, token, and key fetches to the identity provider at login time,
plus an immediate discovery call whenever an admin presses **Test
connection** (that works even while SSO is still disabled — it is how you
prove connectivity before enabling). Startup, local login, and probing
never contact the provider. Builds and images remain fully offline either
way.

### Configure the identity provider (Keycloak example)

1. In your realm, create a **confidential** OpenID Connect client, e.g.
   `polarbeam`. Enable the standard (authorization code) flow; no other
   flow is needed.
2. Set the client's redirect URI to exactly:

   ```text
   https://<dashboard-name>/api/v1/auth/oidc/callback
   ```

3. Note the client secret from the client's credentials tab.
4. To grant PolarBEAM admin from group membership, add a **Group
   Membership** mapper (claim name `groups`, full path off is simplest) to
   the client, and put your operator accounts in a group such as
   `polarbeam-admins`.

### Configure PolarBEAM

Sign in as a local admin and open **Settings → Authentication** (user menu
→ Settings). The fields:

| Field | Meaning |
|---|---|
| Issuer URL | The provider's issuer, e.g. `https://keycloak.example/realms/main`. Discovery runs against `<issuer>/.well-known/openid-configuration`. |
| Client ID / Client secret | The confidential client's credentials. The secret is write-only: after saving, the form shows only that one is stored, and leaving the field empty on later saves keeps it. Changing the issuer or client ID requires entering a new secret — the stored one belongs to the previous provider and is never sent elsewhere. |
| Redirect URL | The exact callback URL above. It is deliberately explicit — the server sits behind an SNI passthrough proxy and never guesses its own external name. |
| Scopes | Must include `openid`. Add `profile`/`email` if your username claim needs them. |
| Username claim | The ID-token claim shown as the dashboard username (default `preferred_username`). Missing claim = failed login, loudly. |
| Role claim / Admin values | The claim checked for admin (default `groups`, string or array). Exact matches against any admin value grant `admin`; every other authenticated user is a `viewer`. |
| Identity provider CA | Optional PEM certificate(s) for providers on a private PKI. When set it replaces the system trust store for IdP calls only. |

Use **Test connection** before enabling: it runs discovery with the
submitted values and reports the provider's endpoints, or the exact
network/TLS error. Saving applies immediately — no restart.

### Semantics worth knowing

- **Settings → Users** (admin-only) lists every account — local and
  federated — with its role, auth source, sign-in count, last sign-in,
  and a 12-month sign-in activity chart, filterable by username, role,
  status, and source. Deleted accounts stay listed (status "Deleted",
  last-known details) as long as their sign-in history is retained. The
  view is read-only; account changes still go through the CLI and SQL
  levers described here.
- Federated users are created on first successful login, keyed on the
  pair of issuer and the provider's immutable `sub` claim — renaming a
  user at the IdP renames it here on the next login instead of creating a
  duplicate. If the username is already taken by another account, the
  federated account instead gets a deterministic `-<8 hex>` suffix
  derived from the issuer and subject. (If that exact suffixed name is
  also taken — practically only when someone pre-created it on purpose —
  the login fails loudly rather than guessing further.)
- Username and role are refreshed from the IdP at every login, and the
  refreshed role takes effect immediately for **all** of that user's open
  dashboard sessions — sessions read the user row on every request, so a
  demotion (or promotion) at the IdP lands everywhere as soon as the user
  next completes an SSO login.
- To revoke one federated user's access without touching the IdP, set its
  `disabled` flag (same lever as local users; there is no CLI for it yet):

  ```sh
  docker compose exec timescaledb psql -U polarbeam -d polarbeam -c \
    "UPDATE users SET disabled = true WHERE username = '<name>'"
  ```

  Disabling kills the user's existing sessions on their next request and
  survives re-login attempts.
- Federated users cannot sign in with a password; the local form treats
  them as unknown users.
- Changing the issuer or client ID signs out every SSO session
  immediately (their roles came from the previous provider); local
  sessions are unaffected. An SSO login already in flight across the
  switch fails loudly instead of completing — it authenticated against
  the previous provider. Accounts from the previous provider stay in
  the database but are unreachable — identities are scoped to the issuer,
  so nobody can sign in to them through the new provider. To tidy them
  up:

  ```sh
  docker compose exec timescaledb psql -U polarbeam -d polarbeam -c \
    "DELETE FROM users WHERE auth_source = 'oidc' AND oidc_issuer <> '<current issuer>'"
  ```
- The client secret lives in the `oidc_settings` table and therefore in
  database backups. Treat backups accordingly, and rotate the secret at
  the provider if a backup leaks.

## Firewall requirements

### Control-plane host

| Direction | Protocol/port | Peer | Purpose |
|---|---|---|---|
| Inbound | TCP 443 | operator browsers | dashboard HTTPS using `<dashboard-name>` |
| Inbound | TCP 443 | every agent | enrollment, config, results, and renewal using `<grpc-name>` |
| Outbound | HTTPS to the IdP's actual ports | identity provider | only with OIDC SSO: discovery/token/JWKS calls at login time, and discovery when an admin runs Test connection (even before enabling). Cover the issuer URL's port (443 unless configured otherwise) and every host/port its discovery document advertises for the authorization, token, and JWKS endpoints. |

The control plane initiates no PolarBEAM connections to agents. Agent config
uses a long-lived TLS connection with keepalive traffic about once a minute.
Stateful middleboxes must allow long-lived connections and must not reap flows
idle for less than about two minutes, or agents reconnect repeatedly.

### Agent hosts

Outbound requirements depend on configured probes:

| Protocol/port | Peer | Purpose |
|---|---|---|
| TCP 443 | control plane | all agent-to-server traffic |
| ICMP echo request | peer agents/targets | latency, loss, and jitter |
| TCP target port | peer agents/targets | TCP and TLS probes |
| TCP URL port | external target | HTTP(S) probes |
| UDP 53 or configured port | DNS target/resolver | DNS probes; no TCP fallback |
| UDP 33434-33523 | peer agents/targets | traceroute probes |

Inbound requirements for mesh destinations are:

| Protocol/port | Purpose |
|---|---|
| ICMP echo request | peers' ICMP probes; the kernel replies |
| TCP mesh-template ports | peers' TCP/TLS probes against a real local service |
| UDP 33434-33523 | peers' traceroutes; the kernel's ICMP port-unreachable reply marks destination reached |

Allow related ICMP echo replies, time-exceeded messages, and port/host
unreachable messages back to the probing host. Blanket inbound ICMP drops
break ICMP and traceroute. IPv6 targets require the ICMPv6 equivalents, and
ICMPv6 must not be blanket-dropped.

Agents expose no operator-facing management port.

## Troubleshooting

### Agent cannot enroll or reconnects continuously

- Confirm `server.address` resolves and TCP 443 is reachable from the agent.
- Confirm `server.sni`, `listen.grpc_hostname`, and
  `POLARBEAM_GRPC_SNI` are identical.
- Check the proxy log for the SNI and `backend=server_grpc`.
- Confirm the token is unexpired, unused, and was created for the intended
  site.
- Confirm the fingerprint exactly matches the one printed with the token.
- Check middlebox idle timeouts if logs repeatedly show `config stream failed`.

### Dashboard works but agents do not

This usually means the default proxy route works but the gRPC SNI route does
not. Recheck all three gRPC-name settings and DNS. The proxy must pass TLS
through; an upstream load balancer that terminates and replaces TLS breaks
agent mTLS unless it is specifically configured for TCP passthrough.

### Neither dashboard nor agents can connect (PROXY protocol mismatch)

The proxy and the server must agree on `listen.proxy_protocol`. Every
combination of disagreement fails immediately and loudly, never silently:

- Proxy sends the header, `proxy_protocol: false` (or an old server binary):
  the header bytes corrupt the TLS handshake — browsers show a protocol
  error, agents log handshake failures, for every connection.
- Proxy does not send it, `proxy_protocol: true`: the server rejects every
  connection at the header read.
- Old server binary with the new key in `server.yaml`: startup fails naming
  `listen.proxy_protocol` as an unknown key.

The bundled proxy and `server.example.yaml` enable it together. Note that
with the knob on, connecting to the server while bypassing the proxy (for
example `curl https://server:8080` from inside the compose network) is
rejected by design — debug through the proxy.

### Agents connect but mesh results are absent or target the proxy

The agent was probably enrolled without the correct `--probe-address`.
Re-enroll with a fresh token and a peer-reachable address. Enrollment refuses
to overwrite identity state; stop the agent and deliberately remove only that
agent's `<state_dir>/pki` directory or replace its dedicated state volume
before re-enrolling. Treat this as identity replacement, not routine repair.

Also verify the WAN firewall permits the selected probe protocol in both
directions.

### Agent fails its pre-start check

```sh
docker logs polarbeam-agent
```

The container entrypoint runs `selfcheck` before `run`, and a failed check
stops the container. The check names the broken YAML key, state-directory
permission, expired identity, private-key mode, or missing socket
capability, each with a remedy.

### Container agent cannot write its state volume

The image runs as UID 10001. Use a dedicated volume initialized by the image,
and do not pre-populate its files as root. Confirm the same volume is mounted
for enrollment and the long-running container.

### Container agent exits with "Operation not permitted"

```text
exec container process `/usr/local/bin/polarbeam-agent`: Operation not permitted
```

The container is missing `NET_RAW`. The binary carries the `cap_net_raw+ep`
file capability, and `execve` fails when that capability is outside the
container's bounding set, so the failure happens before any PolarBEAM code
runs — subcommands that never touch a raw socket, such as `enroll`, fail
identically. Add `--cap-add NET_RAW` (or `cap_add: [NET_RAW]` in Compose) to
every invocation of the image. To confirm the runtime is the cause, read the
bounding set your daemon hands out by default:

```sh
docker run --rm --entrypoint sh \
  ghcr.io/devalexllc/polarbeam-agent:<version> -c 'grep CapBnd /proc/self/status'
```

This uses the agent image, which an air-gapped host already has, rather than
pulling an unrelated one. Overriding the entrypoint is what makes it work
without `--cap-add`: the shell carries no file capabilities, so it executes
under any bounding set — only the agent binary is refused.

`NET_RAW` is capability 13, so the printed mask must have bit `0x2000` set. A
default Docker daemon prints `00000000a80425fb`; the same daemon with
`NET_RAW` removed prints `00000000a80405fb`. If bit `0x2000` is clear, the
runtime is dropping it and every `polarbeam-agent` container needs the flag.

### Zombie `ssl_client` processes accumulate on the control-plane host

Affects control planes running a server image at or before v0.2.1. That
image's healthcheck used BusyBox `wget` over HTTPS, which forks `ssl_client`
and exits without reaping it. The orphan is reparented to the container's PID
1 — `polarbeam-server` itself — which does not reap, so each 30-second check
left one permanent zombie on the host process table (about 2 per minute, or
2,900 per day). They are harmless individually but consume PID-table slots
indefinitely.

Confirm the parent is the server container:

```sh
ps -eo pid,ppid,stat,comm | awk '$3 ~ /Z/'
docker inspect --format '{{.State.Pid}}' <server-container>
```

Any release after v0.2.1 fixes it: the healthcheck is now an in-process
`polarbeam-server healthcheck` that forks nothing. Zombies are reaped only
when PID 1 exits, so the upgrade's container recreation clears the existing
backlog; no host reboot is required.

## Certificate lifecycle

- Agent certificates are valid for 30 days by default and renew automatically
  at two-thirds of their actual lifetime. No normal operator action is needed.
- An agent offline longer than its certificate lifetime cannot renew. Its
  selfcheck reports expiration; re-enroll it with a fresh token.
- The gRPC server certificate is auto-issued by the built-in CA and rotates in
  process.
- The dashboard certificate is operator-managed. Replace the files in the
  `tls` volume and restart the server when renewing it.
- The database is the sole agent-certificate revocation authority. There is no
  CRL or OCSP endpoint.

There is not yet a certificate-revocation CLI. To revoke a known serial:

```sh
docker compose exec timescaledb psql -U polarbeam -d polarbeam -c \
  "UPDATE certificates SET revoked_at = now() WHERE serial = '<serial>'"
```

Existing agent streams using that certificate are dropped within about 30
seconds.

## Upgrades

### Control plane (live upgrade)

A control-plane upgrade is: back up, fetch the new images, apply database
migrations using the new image, then recreate the services. The running
services keep serving until the final step, so the dashboard outage is
only the container recreation itself. Production servers never migrate
automatically — the explicit `migrate` step below is mandatory, and the
system enforces it: a new server started against an unmigrated database
refuses to run with `preflight: database schema is behind … run
'polarbeam-server migrate' first` instead of limping or touching data.

Read the release notes before starting: a release may add required
`server.yaml` keys (for example `listen.proxy_protocol: true`, which the
proxy and server must agree on). Apply such config edits before the final
recreate step so the new containers start against a matching configuration.

Work from the compose directory on the control-plane host:

1. **Back up the database** (see [Backup scope](#backup-scope)). The
   upgrade steps themselves modify no stored measurements or users, but a
   migration can install *retention policies* that later delete history
   past the documented horizons (raw results after 14 days, hourly
   aggregates after 100, daily after 400 — installed by migration 0010).
   If your upgrade crosses a release that adds retention, this backup
   becomes the only copy of anything older, so take it seriously.

2. **Update the pinned version** in `.env`:

   ```sh
   # .env
   POLARBEAM_VERSION=v<new-version>
   ```

3. **Fetch the new images.** Online:

   ```sh
   docker compose pull server proxy timescaledb
   ```

   Offline: load the new bundle's image archive instead
   (`docker load -i images/polarbeam-images-<version>-<arch>.tar` from
   the extracted bundle directory, as in
   [section 2](#2-obtain-the-release-files-and-images)). The TimescaleDB
   image is not in the bundle; you only need to transfer it again when
   the new bundle's `TIMESCALEDB-IMAGE` file names a different digest
   than the one already loaded on the host.

   Pulling or loading only stages the images — everything is still
   running the old version at this point.

4. **Apply pending migrations with the new image.** The old server may
   keep serving while this runs, and only not-yet-applied files run
   (rerunning is a no-op). Most migrations apply and record atomically in
   one transaction; the aggregate migrations (`*.notx.sql`, e.g.
   0002–0003) must run outside one and are written idempotently instead —
   if a run is interrupted around them, simply rerun `migrate` and it
   converges:

   ```sh
   docker compose run --rm server migrate \
     --config /etc/polarbeam/server.yaml
   ```

   This starts a one-off container from the *new* server image (that is
   why step 3 comes first), applies whatever is pending, prints each
   file as it lands, and exits. Large upgrades that backfill aggregates
   may need a larger `migrate --timeout` (default 30 m); do not
   interrupt a running backfill.

5. **Recreate the services on the new version:**

   ```sh
   docker compose up -d
   docker compose ps
   ```

   Do this promptly after step 4 so the serving binary matches the
   migrated schema.

6. **Verify:** `https://<dashboard-name>/healthz` answers, the dashboard
   **Agents** page shows agents reconnecting (they retry on their own —
   no agent-side action is needed for a control-plane upgrade), and
   `docker compose logs --tail=50 server` is free of errors.

### Agents

Agents and the control plane may be upgraded independently, in either order,
because protocol changes are additive: an old agent keeps working against a
new server, and a new agent keeps working against an old one. Upgrade agents
one site at a time so a bad release is visible on the dashboard before it
reaches the whole fleet.

An agent is upgraded by **replacing the container, not the volume**.
The image holds only the binary; everything that must survive lives outside
it — the agent's identity (private key and certificate) and its offline spool
are in the state volume, and its configuration is in the bind-mounted
`agent.yaml`. Recreating the container therefore needs no new token and no
re-enrollment.

Run these on the agent host. They assume the container name, config path, and
volume from [section 8](#8-deploy-a-container-agent); substitute your own if
they differ.

1. **Record what is running now**, so a rollback has an exact target:

   ```sh
   docker inspect --format '{{.Config.Image}}' polarbeam-agent
   ```

2. **Get the new image.** Online:

   ```sh
   docker pull ghcr.io/devalexllc/polarbeam-agent:<new-version>
   ```

   Offline: transfer the agent image tar as in
   [section 2](#offline-installation) and `docker load -i` it here. Either
   way the running agent is untouched at this point — it keeps probing on the
   old image until step 3.

3. **Stop the old container, then remove it.** Two commands, not
   `docker rm -f`:

   ```sh
   docker stop -t 60 polarbeam-agent
   docker rm polarbeam-agent
   ```

   `docker stop` sends SIGTERM, which the agent handles: it waits for
   in-flight probe runs to return, then fsyncs the active spool segment
   before exiting. `docker rm -f` sends SIGKILL instead, which can land
   between the three writes that make up one spool record — the next start
   detects the bad checksum and truncates the torn tail, so the spool
   self-heals, but that one in-flight result is gone.

   Raise `-t` above your longest configured probe timeout. Docker's default
   grace is 10 seconds and then it escalates to SIGKILL on its own,
   which reopens exactly the window the graceful stop was meant to close.
   Shutdown cannot finish faster than the probe still running when SIGTERM
   arrives, and a probe is not always interruptible — the DNS prober sets
   its socket timeouts from the run deadline and cannot be cut short
   mid-exchange against an unresponsive resolver. The 30-second traceroute
   in [section 10.2](#102-add-baseline-icmp-and-traceroute-probes) already
   exceeds the default on its own, so `-t 60` covers this guide's own
   examples; adjust it if you configured longer timeouts.

   Neither command endangers the state volume: `docker rm -v` removes only
   *anonymous* volumes, and `polarbeam-agent-state` is named. Destroying it
   takes a deliberate `docker volume rm`.

4. **Recreate it on the new tag**, with the same flags as the original
   `docker run` — only the image tag changes:

   ```sh
   docker run -d \
     --name polarbeam-agent \
     --restart unless-stopped \
     --cap-add NET_RAW \
     --mount type=bind,src=/opt/polarbeam-agent/agent.yaml,dst=/etc/polarbeam/agent.yaml,readonly \
     --mount type=volume,src=polarbeam-agent-state,dst=/var/lib/polarbeam-agent \
     ghcr.io/devalexllc/polarbeam-agent:<new-version>
   ```

   `--cap-add NET_RAW` is still mandatory — see
   [section 8.2](#82-enroll-into-the-persistent-volume) for why the container
   cannot even start without it. Forgetting `--mount type=volume` is the easy
   mistake here, and it is loud rather than destructive: the new container
   comes up with no identity and fails, but `polarbeam-agent-state` is
   untouched. Remove the failed container and recreate it with the mount —
   identity and spooled results come back intact. Do not re-enroll to escape
   this; a fresh enrollment would replace a working identity for no reason.

5. **Verify:**

   ```sh
   docker exec polarbeam-agent polarbeam-agent version
   docker exec polarbeam-agent polarbeam-agent selfcheck \
     --config /etc/polarbeam/agent.yaml
   docker logs --tail=50 polarbeam-agent
   ```

   The logs should show the agent connecting and receiving a config snapshot
   within about 30 seconds. Confirm on the dashboard's **Agents** page that
   the site's last-update time is advancing again.

Steps 3 and 4 are a measurement gap, not a spool: the agent cannot measure
while it does not exist, so those probe intervals are simply missing from
history. Run the two steps back to back. Results already spooled from an
earlier disconnection are in the volume and replay after the new container
connects.

To roll back, repeat steps 3 and 4 with the tag recorded in step 1. The state
volume is unchanged by an upgrade, so a downgraded agent resumes with the same
identity.

If an agent runs under its own Compose file rather than `docker run`, the
equivalent is to update the pinned tag and then
`docker compose up -d --force-recreate polarbeam-agent`. Give that service a
`stop_grace_period` matching the `-t` value above, since Compose applies its
own 10-second default when the key is absent. Never run
`docker compose down -v` on an agent host: that deletes the identity volume
and forces a re-enrollment with a fresh token.

## Backup scope

A recoverable control-plane backup must include all three persistent Compose
volumes **and** the installation directory's configuration:

- `dbdata`: sites, agents, users, probe configuration, results, and events
- `server-state`: the built-in CA private key and issued gRPC state
- `tls`: the operator-managed dashboard certificate and key
- `.env` and `server.yaml`: the database password, pinned version, and SNI —
  a restored `dbdata` volume keeps the password the role was created with
  (`POSTGRES_PASSWORD` only applies on first initialization), so without the
  original `.env`/`server.yaml` the server cannot reconnect after a restore
  short of manual database recovery

Protect the database, CA, and configuration secrets as one security
boundary. Restoring the database without the matching built-in CA, or the CA
without its database certificate records, does not reproduce the original
agent trust state.
