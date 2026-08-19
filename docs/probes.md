# PolarBEAM probe reference

PolarBEAM agents measure network and service reachability from the sites where
they run. The current agent supports eight probe types:

- ICMP
- TCP
- TLS
- HTTP and HTTPS
- DNS
- NTP
- traceroute
- Path MTU

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
| NTP | UDP only | Target port, default 123 | UDP NTP response |
| traceroute | Outbound UDP plus inbound ICMP | UDP 33434-33523 | ICMP time-exceeded or destination-unreachable |
| Path MTU | ICMP or ICMPv6 with DF set | None | ICMP echo reply, Fragmentation Needed, or Packet Too Big |

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
| UDP 123 or configured port | NTP server target | NTP probe; no TCP fallback |
| UDP 33434-33523 | Peer or external target | traceroute |
| ICMP echo with DF set | Peer or external target | Path MTU probing |

Stateful firewalls must permit corresponding return traffic. ICMP and
traceroute also require the relevant echo-reply, time-exceeded, and
destination-unreachable messages. Path MTU probing additionally requires
inbound ICMPv4 Fragmentation Needed (type 3 code 4) and ICMPv6 Packet Too Big
(type 2), and these arrive from intermediate routers along the path, not just
the target address — a policy that drops them makes every path look like a
PMTU black hole. The IPv6 equivalents are required for IPv6 targets. A blanket
ICMP or ICMPv6 deny policy breaks these probes.

Mesh members are destinations as well as sources, and probe traffic never
crosses networks: a mesh pairs only agents on its own network, so each plane's
firewall envelope covers only that plane's probe addresses. When sites filter
traffic independently, each member's probe address must also accept the
corresponding inbound traffic from its same-network peers:

| Protocol/port | Purpose |
|---|---|
| ICMP echo request | Peers' ICMP and path MTU probes; the kernel replies |
| TCP mesh-template ports | Peers' TCP and TLS probes against a real local service |
| UDP 33434-33523 | Peers' traceroutes; the port-unreachable reply marks the destination reached |

If a target is a hostname rather than an IP address, the agent host must also
be able to reach its normal system resolver. That lookup is separate from a
configured PolarBEAM DNS probe.

PolarBEAM agents expose no operator-facing management port and do not open a
generic probe-listener port. A mesh TCP or TLS probe succeeds only when a real
service already listens on the configured port at each peer's probe address.

## Assignment models

Every probe is either a mesh template or a direct assignment, and every probe
runs on exactly one network. A network is a named connectivity plane: each
agent joins one permanently through its enrollment token, and probes only ever
run between or on agents of their own network. Deployments with one flat
network can ignore the dimension — everything lives on the seeded `default`
network and behaves exactly as described below.

### Mesh probes

A mesh groups sites that should probe one another over one network. A probe
template expands over ordered directions, so a two-site mesh produces both
`site-a -> site-b` and `site-b -> site-a` measurements. A mesh must contain at
least two sites before a probe can be added.

Expansion is per agent, not merely per site, and pairs only agents on the
mesh's network. If site A has two agents and site B has three, all on the
mesh's network, one mesh template produces six A-to-B and six B-to-A
agent-pair series; if one of site B's agents belongs to a different network,
the template produces four and four instead — that agent is never paired,
because the mesh asserts reachability only within its own plane. Each
destination uses the peer agent's `probe_address`, recorded at enrollment.
A mesh's network binding is fixed at creation (`mesh create --network <name>`,
default `default`) and cannot span planes: two meshes on different networks
can cover the same member sites without ever probing across the boundary.

Mesh templates support ICMP, TCP, TLS, DNS, traceroute, and Path MTU. HTTP and
NTP are direct only: HTTP requires a complete target URL, and an expanded NTP template
would query peer agents that do not serve time.

TCP and TLS mesh templates require a `port` parameter between 1 and 65535.
Every destination probe address must have a suitable service listening on that
port. The PolarBEAM agent does not provide that service.

### Direct probes

A direct probe assigns every agent on the probe's network at one source site
to one named external target (`probe add --site <site> --target <name>
[--network <name>]`, default `default`). Agents at the site on other networks
never run it — without that scoping, an operator's external checks would
silently start running from another plane's hardware the day its first agent
enrolls at a shared site. Enrollment-managed agent targets cannot be selected
directly; use a mesh for peer agents.

External targets contain a unique name and one or both of these endpoint
forms:

- An address with an optional port
- A full URL

Use an address for ICMP, traceroute, and Path MTU, an address plus port for
TCP and TLS, an address with an optional port for DNS and NTP, and a full URL
for HTTP or HTTPS.

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
| Network | The plane the probe measures: a direct probe's own setting; mesh probes inherit the mesh's |
| Type | One of the eight supported types |
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
- Path MTU size bounds within 68–9216 bytes with `mtu.min` below `mtu.max`,
  defaults substituted for whichever bound is not set

Train count and spacing affect ICMP. The other current prober implementations
do not use those fields.

Probe type, assignment, and network are immutable because changing them would
combine unrelated measurements under one historical identity. To point a probe
at a different target, move it to another plane, or change its type, delete
the probe and create another.
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
- Total request time, including draining the response body (up to 1 MiB)
- Whether the response status matched the expectation

| Parameter | Default | Meaning |
|---|---|---|
| `http.method` | `GET` | Request method |
| `http.expect_status` | `200` | Exact status such as `200`, or class such as `2xx` |
| `http.insecure_skip_verify` | `false` | Skip HTTPS certificate verification |

Redirects are not followed. A configured endpoint returning 302 is measured as
302 rather than as the destination of its `Location` header. Each run uses a
new connection, and response-body reading is limited to 1 MiB. Reaching that
limit on a larger body is success, but a body that fails before it completes
is not: a transfer that stalls past the probe timeout reports `TIMEOUT`, and
a truncated or reset transfer reports `ERROR`.

Typical uses include:

- End-to-end load balancer and application health checks
- Regional time-to-first-byte comparison
- Detection of an application returning 500 over a healthy TCP/TLS path
- Validation of internal or external anycast endpoints
- Testing a reverse proxy and its upstream application together

Current limitations include no response-body assertion, custom request
headers, authentication, request body, or redirect following. The absence of
a body assertion means body *content* is not checked; body *transfer*
failures (stall, truncation, reset) still fail the probe as described above.

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

## NTP

NTP sends one NTPv4 client-mode request over UDP per run and validates the
reply. It does not fall back to TCP and does not retransmit within a run; the
probe interval is the retry. The probe resolves hostnames to one address,
preferring IPv4 when both families are available, and reports one packet sent,
one received, and the request/response round-trip time.

NTP is direct only. Each site selects its own external target, so different
sites can verify different time servers, and the probe runs only on agents on
the probe's network at its selected source site. Because sites commonly use per-site NTP endpoints
and peer agents do not serve time, NTP cannot be configured as a mesh
template.

NTP has no type-specific parameters. The probe queries the target address,
using the target's port when nonzero and UDP 123 otherwise.

A run is `OK` only when a reply from the queried server:

- Is at least 48 bytes
- Uses server mode
- Echoes the request's transmit timestamp in its originate field — the agent
  fills that field with a random value, so a matching reply proves the server
  answered this specific request
- Reports a stratum from 1 through 15
- Does not report an unsynchronized leap state
- Carries a nonzero transmit timestamp

No reply within the timeout is `TIMEOUT`, and a failed hostname resolution is
`DNS_FAILURE`. A closed UDP port that answers with ICMP port-unreachable is
`CONN_REFUSED`. Every other outcome is an `ERROR` naming the reason, including
malformed or truncated packets, non-server modes, out-of-range strata,
unsynchronized servers, and Kiss-o'-Death replies (stratum 0), which include
the four-character kiss code such as `RATE`, `DENY`, or `RSTR`.

Poll conservatively. Public NTP servers and pool members rate-limit eager
clients and answer with Kiss-o'-Death `RATE`; use an interval of at least 60
seconds unless you operate the time server. Creating or editing an NTP probe
with a faster interval succeeds but returns a warning.

Typical uses include:

- Verifying that each site's approved time source answers NTP from that site
- Detecting a firewall change that silently broke UDP 123 egress
- Confirming a per-site NTP endpoint is synchronized rather than merely up
- Comparing time-service round-trip times across sites or providers

The probe establishes NTP service reachability. It does not compute clock
offset, does not assess time accuracy, does not verify the agent host's own
synchronization, and does not support authenticated NTP or NTS.

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

## Path MTU

Path MTU probing determines the largest IP packet a path carries without
fragmentation. The prober sends ICMP echo requests padded to candidate sizes
with the Don't Fragment behavior enforced (Linux `IP_PMTUDISC_PROBE` on IPv4;
`IPV6_DONTFRAG` on IPv6, where routers never fragment anyway), bounded by a
binary search between `mtu.min` and `mtu.max`. Each size gets up to three
sends before it is judged, and a run tests at most about fifteen sizes — the
search never scans the range. When a router answers with a valid ICMPv4
Fragmentation Needed or ICMPv6 Packet Too Big, the advertised next-hop MTU is
tested directly and, once verified end to end, accepted — the same shortcut
real path MTU discovery uses.

All sizes are IP packet bytes including the IP header, so results compare
directly to interface and link MTUs. A 1500-byte result means a full Ethernet
frame's IP packet passes; the ICMP payload the prober actually sent is 28
bytes smaller on IPv4 (20-byte header + 8-byte echo header) and 48 bytes
smaller on IPv6.

The result contains:

- The largest tested packet size that reached the destination
- The smallest tested size that did not, when one exists
- The next-hop MTU advertised by Fragmentation Needed or Packet Too Big,
  when a plausible one was seen
- The IP version probed
- Whether a PMTU black hole is suspected
- The round-trip time of the successful probe at the largest passing size
- Whether the upper bound came from the local interface rather than the path

Status semantics:

- A converged measurement is `OK`, including a path whose MTU is below
  `mtu.max` — a valid ICMP error answer is a successful measurement, not a
  failure. The pair detail view shows the measured value.
- A path that passes smaller packets while larger ones vanish with no ICMP
  error is a suspected PMTU black hole and reports `TIMEOUT`, never healthy.
  This is the failure mode that breaks VPN, SD-WAN, and overlay traffic while
  pings and TCP handshakes keep succeeding.
- A search that cannot converge inside the run timeout reports `TIMEOUT`
  naming non-convergence.
- Resolution failures report `DNS_FAILURE`; missing privileges, unsupported
  platforms, and local send limits below `mtu.min` report `ERROR` with the
  reason.

The server keeps the latest usable measurement per direction and records an
event whenever the measured MTU or black-hole state changes, mirroring how
traceroute path changes are tracked. Path MTU results never contribute
latency or loss to the pair charts.

Parameters:

| Parameter | Default | Meaning |
|---|---|---|
| `mtu.min` | 1280 | Smallest IP packet size to test, bytes including IP header (68–9216) |
| `mtu.max` | 1500 | Largest IP packet size to test (68–9216, must exceed `mtu.min`) |
| `mtu.family` | prefer IPv4 | Force `4` or `6` when a hostname resolves to both |

On jumbo-frame networks, raise `mtu.max` (for example to 9000): the probe
only reports the largest size it tested, so the default range would report a
clean 1500 on a jumbo path and never look higher. Creating such a probe
prints a non-blocking advisory, because paths beyond the local segment
rarely carry jumbo frames and the reported value will honestly be the
far-end bottleneck. The agent's own egress interface must also carry jumbo
frames — a size the kernel cannot send is reported as a local constraint,
not a path measurement — and since the agent runs in a container, the
**container network's** MTU is what counts. Docker bridge networks default
to 1500 regardless of the host NIC, so run jumbo-probing agents with
`--network host` or on a network created with
`com.docker.network.driver.mtu=9000`. The destination needs no
configuration; its kernel echoes whatever arrives.

Path MTU probes work as mesh templates and direct assignments. Mesh
expansion probes each peer agent's probe address; no port parameter is
needed because the transport is ICMP.

Like traceroute, Path MTU strictly requires `CAP_NET_RAW` (`--cap-add
NET_RAW` on the agent container): unprivileged datagram ICMP sockets cannot
observe the Fragmentation Needed and Packet Too Big errors elicited by the
prober's own packets. The probe additionally uses Linux-only PMTU-probe
socket options, so it reports `ERROR` on other agent platforms. A missing
capability produces an error result on every cadence rather than silently
skipping the probe. `polarbeam-agent selfcheck` reports availability as the
`path_mtu` check.

A timeout of at least 10 seconds is recommended — a worst-case search sends
up to about 45 probes and reports honest non-convergence when the budget is
too small. A typical cadence is five minutes with a 15-second timeout: path
MTU changes when routing or tunnel encapsulation changes, not per packet.

Typical uses include:

- Verify a VPN, SD-WAN, GRE, or VXLAN path carries the MTU applications need
- Detect PMTU black holes where small probes pass and bulk traffic stalls
- Confirm jumbo-frame paths inside a datacenter fabric end to end
- Catch MTU regressions after carrier, tunnel, or overlay changes
- Explain application stalls that ICMP and TCP handshake probes miss

Routers rate-limit the ICMP errors this probe depends on, so a heavily
rate-limited path can occasionally read as a black hole; the three-send
retry absorbs isolated loss but not sustained suppression. On ECMP paths,
probes of different sizes may hash onto different links, in which case the
reported value reflects the smallest MTU among them.

## Result statuses and interpretation

Probe results use these statuses:

| Status | Meaning |
|---|---|
| `OK` | The probe-specific success condition was met |
| `TIMEOUT` | The run exceeded its deadline or received no required response |
| `CONN_REFUSED` | A TCP connection was actively refused, or a UDP probe drew ICMP port-unreachable |
| `DNS_FAILURE` | Resolution, DNS exchange, or expected-RCODE validation failed |
| `TLS_FAILURE` | TLS verification or handshake failed |
| `ERROR` | Another configuration, socket, protocol, or runtime error occurred |
| `UNSUPPORTED` | The assigned probe type is not supported by that agent build |

Timeout classification takes precedence over stage-specific classifications.
For example, a TLS handshake that times out is reported as `TIMEOUT`, not
`TLS_FAILURE`.

The server opens a `probe_failing` outage after three consecutive failures and
closes it after three consecutive successful results. Successful results whose
latency or loss reaches the critical dashboard thresholds (the site pair's
effective values, including any per-pair override; external targets use the
global values) open a `probe_degraded` incident after three consecutive
breaches and close it after three consecutive clean results — warn-tier values
color the dashboard but never open incidents. A series has at most one open
incident: a degraded link that goes fully down escalates to `probe_failing`.
The server also detects agent silence separately, so a disconnected agent is
not mistaken for every probe failing at once.

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

Regional meshes partition by geography; networks partition by plane. When the
same sites host agents on mutually unreachable planes — a management network
and an internet underlay, say — enroll each plane's agents onto their own
network and give each plane its own mesh over the same site set
(`mesh create --name mgmt-wan --network mgmt`) instead of duplicating site
definitions or accepting cross-plane probe failures. The two partitions
compose: `americas-mgmt` can be a regional mesh on the `mgmt` network.

Full-mesh assignment count grows quadratically. With 100 sites and one agent
per site, one template produces 9,900 directional assignments. At a 30-second
cadence, a 10-packet ICMP template produces about 330 runs and 3,300 echo
requests per second across the fleet. Multiple agents per site multiply that
work further. Regional or topology-specific meshes are often more useful than
one unrestricted global mesh. `docs/sizing.md` translates assignment counts
into control-plane hardware and storage requirements.

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

The dashboard's target detail page (Settings → Targets → click the target
name) charts these stages over time: DNS lookup, TCP connect, TLS
handshake, TTFB, and total, averaged per bucket from whichever probe types
measure them (`http` reports the full set; `tls`, `tcp`, and `dns` report
their subsets), alongside per-source-site latency/loss charts and
per-probe health strips.

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
- Direct NTP probes to each site's approved time source: every 60 seconds or
  slower
- Separate regional or topology-specific meshes instead of one global mesh

This layered workload distinguishes IP reachability, packet quality, route
changes, transport reachability, TLS health, application responses, DNS
resolver behavior, and time-service reachability without adding a PolarBEAM
listener to agent hosts.
