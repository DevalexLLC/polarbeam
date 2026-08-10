# PolarBEAM probe reference

PolarBEAM agents measure network and service reachability from the sites where
they run. The current agent supports six probe types:

- ICMP
- TCP
- TLS
- HTTP and HTTPS
- DNS
- traceroute

The probes do not all use the same transport. PolarBEAM uses TCP, UDP, and
ICMP depending on the probe. Probe workloads are configured centrally in the
dashboard or with the `polarbeam-server` CLI; they are not defined in agent
YAML. Connected agents normally receive changes within about 30 seconds.

For deployment and enrollment instructions, see [install.md](install.md). This
document is the detailed reference for what probes do, how they are configured,
and how to use them to validate network paths.

## Protocol and port summary

| Probe | On-wire protocol | Destination port | Return traffic |
|---|---|---:|---|
| ICMP | ICMP or ICMPv6 | None | ICMP echo reply |
| TCP | TCP | Configured target port | TCP return traffic |
| TLS | TLS over TCP | Configured target port | TCP/TLS return traffic |
| HTTP | HTTP over TCP | URL port, normally 80 | TCP/HTTP return traffic |
| HTTPS | HTTP over TLS over TCP | URL port, normally 443 | TCP/TLS/HTTP return traffic |
| DNS | UDP only | Target port, default 53 | UDP DNS response |
| traceroute | Outbound UDP plus inbound ICMP | UDP 33434-33523 | ICMP time-exceeded or destination-unreachable |

ICMP probing is not UDP on the wire. On Linux, the agent first tries an
unprivileged datagram ICMP socket, but the resulting network packets are still
ICMP or ICMPv6 packets. Traceroute is the probe that combines outbound UDP
with inbound ICMP responses.

## PolarBEAM connectivity requirements

### Control-plane traffic

Agents require outbound TCP 443 to the PolarBEAM control-plane proxy. The
control-plane host requires inbound TCP 443 from agents and operator browsers.
The control plane does not initiate PolarBEAM management connections back to
agents.

In the supplied production Compose deployment, TCP 443 is the only publicly
exposed port. The following ports remain inside the Compose network:

| Path | Protocol/port | Purpose |
|---|---|---|
| Proxy to server | TCP 8443 | Agent gRPC and mTLS |
| Proxy to server | TCP 8080 | Dashboard HTTPS |
| Server to TimescaleDB | TCP 5432 | Database traffic |

Do not expose 8080, 8443, or 5432 between sites merely to operate PolarBEAM.

### Probe traffic

Agent egress depends on the configured workload:

| Protocol/port | Destination | Purpose |
|---|---|---|
| ICMP echo | Peer or external target | ICMP latency, loss, and jitter |
| Configured TCP port | Peer or external target | TCP and TLS probes |
| URL-derived TCP port | External target | HTTP and HTTPS probes |
| UDP 53 or configured port | DNS target or resolver | DNS probe; no TCP fallback |
| UDP 33434-33523 | Peer or external target | traceroute |

Stateful firewalls must permit corresponding return traffic. ICMP and
traceroute also require the relevant echo-reply, time-exceeded, and
destination-unreachable messages. The IPv6 equivalents are required for IPv6
targets. A blanket ICMP or ICMPv6 deny policy breaks these probes.

Mesh members are destinations as well as sources. When sites filter traffic
independently, each member's probe address must also accept the corresponding
inbound traffic from its peers:

| Protocol/port | Purpose |
|---|---|
| ICMP echo request | Peers' ICMP probes; the kernel replies |
| TCP mesh-template ports | Peers' TCP and TLS probes against a real local service |
| UDP 33434-33523 | Peers' traceroutes; the port-unreachable reply marks the destination reached |

If a target is a hostname rather than an IP address, the agent host must also
be able to reach its normal system resolver. That lookup is separate from a
configured PolarBEAM DNS probe.

PolarBEAM agents expose no operator-facing management port and do not open a
generic probe-listener port. A mesh TCP or TLS probe succeeds only when a real
service already listens on the configured port at each peer's probe address.

## Assignment models

Every probe is either a mesh template or a direct assignment.

### Mesh probes

A mesh groups sites that should probe one another. A probe template expands
over ordered directions, so a two-site mesh produces both `site-a -> site-b`
and `site-b -> site-a` measurements. A mesh must contain at least two sites
before a probe can be added.

Expansion is per agent, not merely per site. If site A has two agents and site
B has three, one mesh template produces six A-to-B and six B-to-A agent-pair
series. Each destination uses the peer agent's `probe_address`, recorded at
enrollment.

Mesh templates support ICMP, TCP, TLS, DNS, and traceroute. HTTP is direct
only because it requires a complete target URL.

TCP and TLS mesh templates require a `port` parameter between 1 and 65535.
Every destination probe address must have a suitable service listening on that
port. The PolarBEAM agent does not provide that service.

### Direct probes

A direct probe assigns every agent at one source site to one named external
target. Enrollment-managed agent targets cannot be selected directly; use a
mesh for peer agents.

External targets contain a unique name and one or both of these endpoint
forms:

- An address with an optional port
- A full URL

Use an address for ICMP and traceroute, an address plus port for TCP and TLS,
an address with an optional port for DNS, and a full URL for HTTP or HTTPS.

Target creation currently verifies that at least an address or URL exists,
but it does not cross-check the target against a later probe type. For example,
a URL-only target can be created and then selected for a TCP probe, but the
probe will fail because it has no address and port. Operators must choose a
target whose fields match the probe type.

## Common configuration

Every probe has the following settings:

| Setting | Meaning |
|---|---|
| Assignment | A mesh, or a source site plus external target |
| Type | One of the six supported types |
| Interval | Time between scheduled runs |
| Timeout | Maximum duration of one run |
| Train count | Packet count for ICMP trains |
| Train spacing | Delay between ICMP packets |
| Parameters | Probe-specific key/value settings |
| Enabled | Whether agents should run the probe |

The CLI defaults are a 30-second interval and a 5-second timeout. ICMP uses 10
packets and 200 ms spacing when its train settings are zero or omitted.

Probes are created enabled unless the create request says otherwise:
`probe add --enabled=false` on the CLI, or `"enabled": false` in the API
request, creates the probe stopped so it can be enabled later.

Configuration validation requires:

- A positive interval and timeout
- A timeout shorter than the interval
- Nonnegative train count and spacing
- An explicit train count when explicit spacing is supplied
- A train that fits within the timeout — for ICMP this includes the implicit
  default train (10 × 200 ms), so an ICMP probe needs a timeout above two
  seconds unless explicit train settings shrink the train
- Known parameters for the selected probe type
- A port between 1 and 65535 for mesh TCP and TLS probes

Train count and spacing affect ICMP. The other current prober implementations
do not use those fields.

Probe type and assignment are immutable because changing them would combine
unrelated measurements under one historical identity. To point a probe at a
different target or change its type, delete the probe and create another.
Cadence, train settings, parameters, and enabled state can be edited in place,
whether the probe was created enabled or disabled.

Editing a target is different from changing a probe's assignment. Re-adding a
target under an existing name updates its address, port, or URL in place while
keeping the same target identity, so probes assigned to it continue their
historical series across the endpoint change. Create a target under a new name
when measurements of the new endpoint must not be mixed with the old history.

The agent runs one worker per assigned probe. Each worker deterministically
staggers its first run by a per-probe offset of up to about four seconds, never
exceeding the interval, so workers do not all start at the same instant after
an agent restart or configuration update. Later runs keep that offset, so with
long intervals the fleet's runs cluster near the start of each interval rather
than spreading across it.

## ICMP

ICMP sends a train of echo requests and reports:

- Sent and received packet counts
- Packet loss
- Minimum, average, and maximum round-trip time
- Round-trip-time standard deviation
- Smoothed RFC 3550 jitter carried across runs for the probe series

The default train is 10 echo requests spaced 200 ms apart. The prober resolves
hostnames to one address, preferring IPv4 when both families are available.

If at least one echo reply arrives, the run is `OK`; partial loss remains
visible in the packet counts and loss percentage. If no replies arrive before
the deadline, the run is a timeout.

The agent first tries unprivileged datagram ICMP and then a raw socket fallback.
The fallback requires `CAP_NET_RAW` — for the agent container that means
`--cap-add NET_RAW` (Docker grants `NET_RAW` by default; Podman, rootless
daemons, and narrowed `default-capabilities` setups do not). Datagram ICMP
additionally requires `net.ipv4.ping_group_range` on the host kernel to cover
the agent's group. The `polarbeam-agent selfcheck` command reports which modes
are available.

ICMP has no type-specific parameters. Its configuration is the target address,
interval, timeout, and optional train count and spacing.

Typical uses include:

- Baseline site-to-site WAN latency and packet loss
- Directional comparison of primary and return paths
- SD-WAN tunnel monitoring
- Circuit-quality comparison before a failover
- Detection of congestion before application connections completely fail

An ICMP failure does not prove an application path is unavailable. Hosts and
firewalls can suppress echo traffic while allowing application TCP traffic.

## TCP

TCP performs a timed connection to the target address and port. When the
address is a hostname, name resolution is included in `tcp_connect_us`, matching
what an application experiences. A successful result reports TCP connect and
total time.

The prober closes the connection immediately after it succeeds. It does not
send an application request, so success proves the TCP handshake and accept
path, not the health of the service protocol.

Port selection differs by assignment:

- Direct probe: the port comes from the external target.
- Mesh probe: `port` is a required template parameter.

TCP has no other type-specific parameters.

Typical uses include:

- Branch-to-load-balancer reachability on TCP 443
- Application-to-database reachability on TCP 5432
- LDAP, SMTP, SSH, or custom listener monitoring
- Detection of firewall rules that allow ICMP but block an application port
- Distinguishing a timeout from an actively refused connection

## TLS

TLS first makes a TCP connection and then performs a TLS handshake. It reports
TCP connect time, TLS handshake time, total time, and any verification or
handshake error.

| Parameter | Default | Meaning |
|---|---|---|
| `tls.sni` | Target address | Override the handshake server name |
| `tls.insecure_skip_verify` | `false` | Skip certificate and hostname verification |

A mesh TLS probe also requires `port`. Direct probes obtain the port from the
target.

Typical uses include:

- Validate SNI routing through an L4 load balancer
- Detect expired, untrusted, or hostname-mismatched certificates
- Separate TCP latency from TLS handshake latency
- Detect a load balancer returning its default certificate
- Test a common TLS service deployed at every mesh peer address

Avoid `tls.insecure_skip_verify=true` except for deliberately self-signed test
services. The current probe cannot supply a client certificate or custom CA
bundle.

## HTTP and HTTPS

HTTP performs a real request against a complete target URL. It is available
only as a direct probe. The URL scheme determines whether the run uses plain
HTTP or HTTPS and supplies the default port when the URL omits one.

The probe measures:

- DNS lookup time
- TCP connect time
- TLS handshake time for HTTPS
- Time to first response byte
- Total request time
- Whether the response status matched the expectation

| Parameter | Default | Meaning |
|---|---|---|
| `http.method` | `GET` | Request method |
| `http.expect_status` | `200` | Exact status such as `200`, or class such as `2xx` |
| `http.insecure_skip_verify` | `false` | Skip HTTPS certificate verification |

Redirects are not followed. A configured endpoint returning 302 is measured as
302 rather than as the destination of its `Location` header. Each run uses a
new connection, and response-body reading is limited to 1 MiB.

Typical uses include:

- End-to-end load balancer and application health checks
- Regional time-to-first-byte comparison
- Detection of an application returning 500 over a healthy TCP/TLS path
- Validation of internal or external anycast endpoints
- Testing a reverse proxy and its upstream application together

Current limitations include no response-body assertion, custom request
headers, authentication, request body, or redirect following.

## DNS

DNS sends a DNS query over UDP. It does not fall back to TCP. The probe records
the exchange duration and compares the returned RCODE with the expected RCODE.

| Parameter | Required | Default | Meaning |
|---|---|---|---|
| `dns.qname` | Yes | None | Name to query |
| `dns.qtype` | No | `A` | Query type |
| `dns.expect_rcode` | No | `NOERROR` | Expected response code |
| `dns.resolver` | No | Target address and port | Resolver override in `host:port` form |

Supported query types are `A`, `AAAA`, `CNAME`, `MX`, `NS`, `PTR`, `SOA`,
`SRV`, and `TXT`.

Supported expected RCODEs are `NOERROR`, `FORMERR`, `SERVFAIL`, `NXDOMAIN`,
`NOTIMPL`, and `REFUSED`.

When `dns.resolver` is absent, the probe queries the target address. It uses
the target's port when nonzero and UDP 53 otherwise.

The probe validates the response RCODE but does not inspect answer content. A
`NOERROR` response is therefore successful even if an operator expected a
specific address that was not returned.

Typical uses include:

- Corporate resolver availability from branch sites
- Split-horizon DNS reachability
- Active Directory service discovery with SRV queries
- Expected negative responses using `NXDOMAIN`
- Resolver-latency comparison across regions
- Security-zone access to approved resolvers

Use mesh DNS carefully. Without `dns.resolver`, every source queries each peer
agent's probe address on UDP 53. That is useful only when the peer hosts
actually run DNS. A direct probe against a named resolver target is usually
clearer.

## Traceroute

Traceroute sends three UDP probes per hop with an increasing IPv4 TTL or IPv6
hop limit. It listens for raw ICMP time-exceeded and destination-unreachable
messages. It supports up to 30 hops, using UDP destination ports 33434 through
33523.

The result contains:

- Responding addresses at each hop
- Round-trip measurements for responding probes
- Silent hops
- Whether the destination was reached
- A SHA-256 hash of the hop-address sequence

The server compares path hashes to detect path changes. The destination is
considered reached when a port-unreachable response arrives for one of the
run's probes, or when any matching ICMP reply arrives from the target address
itself.

Traceroute strictly requires `CAP_NET_RAW` (`--cap-add NET_RAW` on the agent
container). Unprivileged datagram ICMP sockets cannot receive the ICMP errors
caused by the separate UDP socket. Missing raw socket access produces an error
result on every cadence rather than silently skipping the probe.

Traceroute has no type-specific parameters. A typical cadence is five minutes
with a 30-second timeout.

Typical uses include:

- Detect SD-WAN, MPLS, or carrier route changes
- Record the path before and during an outage
- Compare route changes in opposite directions
- Detect unexpected transit or security-zone detours
- Correlate a latency increase with a changed hop sequence

Traceroute can time out even when an application path works if firewalls,
routers, or the destination suppress the required ICMP responses.

## Result statuses and interpretation

Probe results use these statuses:

| Status | Meaning |
|---|---|
| `OK` | The probe-specific success condition was met |
| `TIMEOUT` | The run exceeded its deadline or received no required response |
| `CONN_REFUSED` | A TCP connection was actively refused |
| `DNS_FAILURE` | Resolution, DNS exchange, or expected-RCODE validation failed |
| `TLS_FAILURE` | TLS verification or handshake failed |
| `ERROR` | Another configuration, socket, protocol, or runtime error occurred |
| `UNSUPPORTED` | The assigned probe type is not supported by that agent build |

Timeout classification takes precedence over stage-specific classifications.
For example, a TLS handshake that times out is reported as `TIMEOUT`, not
`TLS_FAILURE`.

The server opens a `probe_failing` outage after three consecutive failures and
closes it after three consecutive successful results. It also detects agent
silence separately, so a disconnected agent is not mistaken for every probe
failing at once.

## Large-infrastructure patterns

### Regional WAN and SD-WAN meshes

Create separate meshes such as `americas-wan`, `emea-wan`, `apac-wan`, and
`datacenter-backbone`. Run ICMP every 30 seconds and traceroute every five
minutes:

```sh
polarbeam-server probe add --config /etc/polarbeam/server.yaml \
  --mesh americas-wan --type icmp \
  --interval 30s --timeout 5s \
  --train-count 10 --train-spacing 200ms

polarbeam-server probe add --config /etc/polarbeam/server.yaml \
  --mesh americas-wan --type traceroute \
  --interval 5m --timeout 30s
```

This gives directional latency, loss, jitter, and routing history while
keeping unrelated network domains in separate workload boundaries.

Full-mesh assignment count grows quadratically. With 100 sites and one agent
per site, one template produces 9,900 directional assignments. At a 30-second
cadence, a 10-packet ICMP template produces about 330 runs and 3,300 echo
requests per second across the fleet. Multiple agents per site multiply that
work further. Regional or topology-specific meshes are often more useful than
one unrestricted global mesh.

### Layered validation of a critical application

Use TCP, TLS, and HTTP together to identify which layer fails:

```sh
polarbeam-server target add --config /etc/polarbeam/server.yaml \
  --name payments-l4 --address 10.50.20.40 --port 443

polarbeam-server target add --config /etc/polarbeam/server.yaml \
  --name payments-health \
  --url https://payments.internal.example/health

polarbeam-server probe add --config /etc/polarbeam/server.yaml \
  --site chicago --target payments-l4 --type tcp \
  --interval 30s --timeout 5s

polarbeam-server probe add --config /etc/polarbeam/server.yaml \
  --site chicago --target payments-l4 --type tls \
  --interval 30s --timeout 5s \
  --param tls.sni=payments.internal.example

polarbeam-server probe add --config /etc/polarbeam/server.yaml \
  --site chicago --target payments-health --type http \
  --interval 30s --timeout 10s \
  --param http.method=GET --param http.expect_status=2xx
```

Interpret the combination as follows:

- TCP fails: investigate routing, firewall, load-balancer listener, or service
  acceptance.
- TCP succeeds but TLS fails: investigate SNI, certificate, or handshake
  configuration.
- TLS succeeds but HTTP fails: investigate the reverse proxy or application.
- All succeed but TTFB rises: investigate application or backend performance.

Repeat the direct assignments for each source site that needs independent
coverage.

### Corporate DNS from each security zone

Create a resolver target and query both an expected service record and an
expected negative name:

```sh
polarbeam-server target add --config /etc/polarbeam/server.yaml \
  --name corp-dns-east --address 10.20.0.53 --port 53

polarbeam-server probe add --config /etc/polarbeam/server.yaml \
  --site branch-atlanta --target corp-dns-east --type dns \
  --interval 30s --timeout 5s \
  --param dns.qname=_ldap._tcp.dc._msdcs.corp.example \
  --param dns.qtype=SRV --param dns.expect_rcode=NOERROR

polarbeam-server probe add --config /etc/polarbeam/server.yaml \
  --site branch-atlanta --target corp-dns-east --type dns \
  --interval 2m --timeout 5s \
  --param dns.qname=known-absent.corp.example \
  --param dns.qtype=A --param dns.expect_rcode=NXDOMAIN
```

This continuously verifies resolver reachability and response classes from a
representative branch. It does not verify the exact returned record values.

### Firewall and segmentation validation

Place agents in representative user, application, database, management, and
DMZ networks. Add direct TCP probes for paths that policy says must work, for
example:

- Application to database on TCP 5432
- Application to a secrets service on TCP 8200
- Management to hosts on TCP 22
- DMZ to an internal API on TCP 443

This provides continuous evidence that approved paths remain reachable after
firewall changes. PolarBEAM does not currently support an expected-failure
condition. A deliberately blocked connection is treated as a failed probe and
may open an outage, so the current TCP probe cannot represent a denied path as
a successful compliance assertion.

### TLS certificate and SNI coverage

Create direct TLS probes to shared VIPs from several sites, using the service's
real SNI. This can detect expired certificates, missing subject alternative
names, wrong virtual-host routing, default certificates, and region-specific
handshake delays. Pair each TLS probe with TCP when operators need to separate
certificate or handshake failures from basic transport failures.

### Route-change forensics

Combine a frequent ICMP probe, a slower traceroute, and a TCP probe to a
critical service. When latency, loss, or reachability changes, operators can
compare the incident with the saved traceroute path. Separate mesh directions
make one-way carrier or return-path changes visible instead of averaging them
into one site-pair measurement.

## Suggested initial workload

A practical enterprise baseline is:

- ICMP mesh: 30-second interval, 5-second timeout, 10 packets at 200 ms
- Traceroute mesh: 5-minute interval, 30-second timeout
- Direct TCP probes to critical listener ports: every 30-60 seconds
- Direct TLS probes to certificate-sensitive services: every 30-60 seconds
- Direct HTTP health probes: every 30-60 seconds
- Direct DNS probes to approved resolvers: every 30 seconds
- Separate regional or topology-specific meshes instead of one global mesh

This layered workload distinguishes IP reachability, packet quality, route
changes, transport reachability, TLS health, application responses, and DNS
resolver behavior without adding a PolarBEAM listener to agent hosts.
