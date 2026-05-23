# Staging deploy runbook (Fase 0 PR9 / SIN-62215)

This page tells a new on-call engineer how to:

1. provision a staging VPS from scratch in under one hour,
2. read logs while a deploy is in flight,
3. roll back manually when the smoke check goes red,
4. understand which secrets the GitHub Actions CD job expects,
5. bump the SHA256 digest of an infra image (postgres, caddy, redis, nats, minio).

The CD pipeline itself is `.github/workflows/cd-stg.yml`; the staging stack is
`deploy/compose/compose.stg.yml`; the on-host wrapper invoked over SSH is
`deploy/scripts/stg-deploy.sh`.

## Pre-merge gates

Beyond `ci` / `paperclip-lint` / `aicache-lint` / `webhook-integration` (which
all install Go via `actions/setup-go` and never exercise the multi-stage
Dockerfile), one extra gate runs on every PR that touches the staging build
inputs:

| Gate                    | Workflow                                  | Triggers on path change of                                | What it catches                                                                |
| ----------------------- | ----------------------------------------- | --------------------------------------------------------- | ------------------------------------------------------------------------------ |
| `docker-smoke` (SIN-62301) | `.github/workflows/docker-smoke.yml`      | `Dockerfile`, `.dockerignore`, `go.mod`, `go.sum`          | Builder-image vs `go.mod` toolchain drift — fails fast at `go mod download` if the pinned base image cannot satisfy the source tree's `go`/`toolchain` directives. |
| `govulncheck` (SIN-62298) | `.github/workflows/govulncheck.yml`       | every PR                                                  | Reachable stdlib/dep CVEs (call-graph, source-mode).                          |
| `compose-unbound-parity` (SIN-62332) | `.github/workflows/compose-unbound-parity.yml` | `deploy/compose/**`, `deploy/caddy/**`, `infra/caddy/**`, the lint script and its fixtures | Any compose with a Caddyfile `on_demand_tls` catch-all that ships without the Unbound sidecar + `dns: ["unbound"]` pin (F44 / ADR 0079 §2 deploy gate). |

The gates are **complementary, not duplicates**: `govulncheck` runs against the
source tree's import graph; `docker-smoke` runs against the build sandbox the
staging image will actually compile in. Both fail closed.

`docker-smoke` builds the multi-stage `builder` target only — `push: false`,
`load: false`, GHA-scoped buildx cache. The failure mode that motivated the
gate (`[builder 4/7] RUN go mod download` against a builder image that does
not satisfy `toolchain go1.25.9`) surfaces before the runtime stage, so
building further would add wall-clock without adding signal. Cache-hit runs
land under ~1 min; cold builds land under the workflow's 8-min timeout.

**If you bump `go.mod` toolchain or `go` directive, also bump:**

- the builder `FROM` digest in `Dockerfile` (currently
  `golang:1.25.9-alpine@sha256:5caaf1cca…`) — see "Bumping infra image
  digests" below for the resolve-by-digest flow;
- `actions/setup-go` `go-version:` in `.github/workflows/ci.yml` (and the
  other Go-using workflows);
- the dev compose Go base in `deploy/compose/compose.yml` if it is in scope
  for your change.

`docker-smoke` will fail closed on the PR if the Dockerfile builder lags,
which is the postmortem fix-forward from SIN-62297 (the orphan-bridge merge
that was 8/8 green pre-merge and red 23s post-merge). The gate is also the
positive-control demonstration: regressing the builder image in a throwaway
PR turns it red.

## Architecture in one paragraph

Every push to `main` triggers `ci`. When `ci` finishes green, `cd-stg` wakes,
builds the multi-stage distroless image from `Dockerfile`, pushes it to GHCR,
and SSHes into the VPS. The VPS-side wrapper (`/opt/crm/stg/bin/deploy.sh`,
copied from `deploy/scripts/stg-deploy.sh`) rewrites only the `APP_IMAGE=`
line in `/opt/crm/stg/.env.stg`, runs `docker compose pull && up -d`, prunes
dangling images, and exits. The runner then `curl`s `https://${STG_SMOKE_URL}/health`
from outside; failure paints the job red. **No automatic rollback** — Fase 6
will revisit that.

Image policy: every image in `compose.stg.yml`, including the app, is consumed
by SHA256 digest, never by floating tag. `grep -E ':(latest|alpine)$'
deploy/compose/compose.stg.yml` MUST return zero matches; CI fails the build
if it ever does.

### Why the fork (`ia-dev-sindireceita/crm`) does not deploy (SIN-63281)

Only the upstream repo (`pericles-luz/crm`) is allowed to push the staging
image. This is normative — set by the board on SIN-63281 ("Só o repositório
`pericles-luz/crm` pode mandar pra stg"). Mechanically the gate is enforced by
a job-level `if` on both deploy workflows:

| Workflow                                  | Gate                                                                  | What runs on the fork                                |
| ----------------------------------------- | --------------------------------------------------------------------- | ---------------------------------------------------- |
| `.github/workflows/cd-stg.yml`            | `github.repository_owner == 'pericles-luz'`                           | Nothing — the `deploy-stg` job is skipped entirely.  |
| `.github/workflows/build-backup-image.yml`| `github.event_name == 'pull_request' \|\| github.repository_owner == 'pericles-luz'` | PRs validate the build (`push: false`, GHA cache); pushes to fork-`main` are skipped. |

Why this shape rather than a fork-side GHCR namespace:

- The image (`ghcr.io/pericles-luz/crm` / `…/crm-backup`) and the cosign
  identity regex pinned in `deploy/scripts/stg-deploy.sh` are bound to
  `pericles-luz/crm`. Changing namespace would force a coordinated VPS-side
  bump of `EXPECTED_REPO`, `COSIGN_IDENTITY_REGEXP`, GHCR pull credentials,
  and `/opt/crm/stg/.env.stg` — see "GHCR pull credentials" below — and that
  would still leave the supply-chain identity drifting from upstream.
- The fork's `GITHUB_TOKEN` does not have `packages:write` against
  `pericles-luz/*`, so even pinned to the upstream namespace it cannot push.
  Before SIN-63281 this surfaced as a 403 on every `docker push` from the
  fork (`cd-stg` red 96/96 runs from 2026-05-16 to 2026-05-22). With the
  gate, the fork CI is green again and we stop spamming a doomed `docker
  push` per merge.
- The deploy lives on whichever clone of the workflow file runs on
  `pericles-luz/crm` — same YAML, no divergence between fork and upstream.

The supply-chain invariant lint (`tools/supply-chain/test_workflow_invariants.sh`)
asserts the gate is present on both files, so removing it without a
deliberate runbook update fails CI.

**Diagnostic checklist if `cd-stg` 403s on `docker push` again:**

1. Confirm the run is on `pericles-luz/crm`, not the fork. The fork's
   `deploy-stg` job should be `Skipped`, not `Failed`. A `Failed` push from
   the fork means the gate was edited away — `git log .github/workflows/cd-stg.yml`
   and revert.
2. On upstream, confirm `IMAGE_REPO` still reads `ghcr.io/pericles-luz/crm`
   (workflow `env`). Drift here would mean the image is being pushed to a
   namespace whose write-token upstream does not own.
3. If both are correct, the 403 is a real GHCR-side issue (PAT expired,
   package permissions changed). Check
   `https://github.com/users/pericles-luz/packages/container/crm/settings`
   and the cosign keyless OIDC binding — see ADR 0084.

## Custom-domain catch-all (F44 / Unbound) deploy gate

Before any compose published to staging or prod can serve a `:443` catch-all
for tenant custom domains (Caddyfile `on_demand_tls` block), it MUST also
ship the Unbound sidecar AND pin Caddy's container DNS to it. Skipping
either half re-opens the F44 attack class — DNS rebinding via the ACME
HTTP-01 challenge resolves a tenant-controlled name to `127.0.0.1` /
`169.254.169.254` / a private VPC range, and Caddy hands the issuance token
to the attacker (originally raised in the SIN-62226 umbrella, re-validated
in SIN-62328 § R-A).

**Required parity in every `compose*.yml` that mounts a Caddyfile with
`on_demand_tls`:**

1. A top-level service named `unbound`, mounting `infra/caddy/unbound.conf`
   read-only at `/opt/unbound/etc/unbound/a-records.conf`. The blocklist in
   that file mirrors `internal/customdomain/validation/blocklist.go` (drift
   here = security drift).
2. The `caddy` service has `dns: ["unbound"]` so the container's
   `/etc/resolv.conf` points at the sidecar. This is what catches HTTP-01
   challenge name resolution — Caddy's own `tls.resolvers` block only
   covers DNS-01 plugin lookups.
3. `caddy.depends_on` includes `unbound: { condition: service_started }`
   so issuance never fires before the sidecar is up.

`deploy/compose/compose.yml` and `deploy/compose/compose.stg.yml` both ship
the sidecar today even though only the local-dev compose has the catch-all
turned on — staging keeps the parity wired so flipping
`on_demand_tls` in `Caddyfile.stg` later cannot, by itself, regress F44.

**CI gate:** `.github/workflows/compose-unbound-parity.yml` runs
`scripts/check-compose-unbound-parity.sh` over every `deploy/compose/compose*.yml`
on each PR. The script identifies the active Caddyfile per compose
(`caddy.command --config …` falling back to `/etc/caddy/Caddyfile`),
reads it from `deploy/caddy/`, and fails the workflow if the Caddyfile
contains an uncommented `on_demand_tls` directive while the compose lacks
either piece of parity. A fixture-driven self-test (`scripts/check-compose-unbound-parity.test.sh`,
fixtures under `scripts/testdata/compose-unbound-parity/`) ensures the
lint itself does not silently rot. Expect both jobs to be green before
merging any change to `deploy/compose/**`, `deploy/caddy/**`, or
`infra/caddy/**`.

**Manual smoke test (post-deploy):** confirm Unbound refuses
private/loopback answers from inside the compose network — this is the
direct mitigation for F44, so it should be re-run any time the sidecar
config changes.

```bash
COMPOSE_ARGS="--env-file /opt/crm/stg/.env.stg -f /opt/crm/stg/compose.stg.yml"

# Spin up a throwaway tool container on the same network and ask Unbound
# to resolve a host that, hypothetically, points at a private IP. The
# `private-address` + `deny-answer-address` blocks in unbound.conf must
# rewrite the answer to NXDOMAIN/REFUSED — anything else is a regression.
sudo -u crm-deploy docker compose ${COMPOSE_ARGS} run --rm \
  --no-deps --entrypoint sh caddy \
  -c 'apk add -q drill && drill @unbound -p 5353 localhost'
# Expect: ;; ->>HEADER<<- opcode: QUERY, rcode: NXDOMAIN  (or REFUSED)
```

Re-running this from the runbook lets you verify staging parity even
before flipping `on_demand_tls.ask` on `Caddyfile.stg`. If the answer is
`NOERROR` with `127.0.0.1` in the answer section, do NOT proceed with a
catch-all flip — the sidecar is misconfigured.

## HTTP edge / IP trust boundary

The per-IP rate limit on `GET /c/{slug}` (and any future per-IP cap) keys
off `r.RemoteAddr` as rewritten by `chi.RealIP`. That rewrite is only safe
when (a) the edge strips client-supplied identity headers before forwarding
and (b) the app only honours those headers when the immediate TCP peer is
a trusted proxy. Both controls already ship — Caddy strips on the tenant
vhost (`deploy/caddy/Caddyfile`, `deploy/caddy/Caddyfile.stg`) and the
Go side wraps `chi.RealIP` with a trust gate
(`internal/adapter/httpapi/trusted_realip.go`). What is documented below
is the operator-facing surface: the three places where a future infra
change can silently re-open the bypass without changing any application
code.

### 1. `TRUSTED_PROXY_CIDRS` defaults and overrides

The trust gate's default allowlist is loopback plus the two RFC1918 ranges
docker compose uses for its bridge networks and that operators commonly
assign to internal L4 LBs:

| CIDR             | Why it is in the default                                                |
| ---------------- | ----------------------------------------------------------------------- |
| `127.0.0.1/32`   | Local smoke checks (`curl http://localhost:8080/health`) inside the VPS. |
| `::1/128`        | Same, IPv6 loopback.                                                    |
| `172.16.0.0/12`  | Docker compose user-defined bridge networks (production layout).        |
| `10.0.0.0/8`     | RFC1918 range commonly used for internal L4 load balancers.             |

In the production layout (Caddy and the app share a compose network inside
`172.16/12`) the immediate TCP peer is always inside the default set, so
no override is needed.

**When to override.** Set `TRUSTED_PROXY_CIDRS` (process env, e.g. an
extra line in `/opt/crm/stg/.env.stg`) when the immediate TCP peer is
NOT inside the default set. The two realistic cases:

- The reverse proxy / LB lives on a public-IP subnet outside RFC1918.
  Append that proxy's egress CIDR — pin it as tightly as the LB's
  documentation allows (e.g. a single `/32` if the LB has a static IP).
- The container network uses a non-default bridge range outside
  `172.16/12` (uncommon — only happens if the docker daemon was
  reconfigured with `default-address-pools` or a custom network was
  declared with an explicit `subnet:` outside the RFC1918 ranges
  baked into the default).

**Format.** Comma-separated CIDRs, e.g.
`TRUSTED_PROXY_CIDRS=10.0.0.0/8,192.0.2.10/32`. Whitespace around each
entry is trimmed. Setting the variable replaces the defaults entirely —
if you still want loopback + `172.16/12` honoured, list them explicitly.

**Safe degrade — read this before debugging.** Invalid CIDR entries are
silently dropped at boot. If every entry in the override is invalid
(e.g. `TRUSTED_PROXY_CIDRS=bogus` or
`TRUSTED_PROXY_CIDRS=10.0.0.0` with the prefix length forgotten), the
wrapper falls back to the documented defaults rather than disabling
trust or refusing to start. The practical consequence for operators:

- `TRUSTED_PROXY_CIDRS=` (empty) → defaults apply.
- `TRUSTED_PROXY_CIDRS=bogus` → defaults apply. The parse drop is silent
  (no log, no startup warning) — the wrapper just falls back.
- `TRUSTED_PROXY_CIDRS=10.0.0.0/8,bogus` → only `10.0.0.0/8` applies; the
  defaults are NOT re-added.

If the override appears to be ignored, do not assume the env var did not
reach the process — silent fallback to defaults on a fully-invalid input
is the documented behaviour, not a bug. The fastest live verification is
to issue a request from an IP just outside what you intended to trust and
observe whether `X-Forwarded-For` gets honoured downstream; if it is,
your override either parsed clean or fell back to defaults that happened
to include the test source.

The authoritative reference is the doc-comment on
`internal/adapter/httpapi/trusted_realip.go`; this runbook only restates
the operator-facing summary.

### 2. The local-dev `:8080` listener is loopback-only

`deploy/caddy/Caddyfile` defines a plain-HTTP site block for local smoke
checks:

```caddy
:8080 {
  import security-headers.caddy
  reverse_proxy app:8080
}
```

That block does NOT carry the three `request_header -…` strip lines that
the tenant vhost (`*.crm.local, crm.local`) carries. It exists so a
developer can `curl http://localhost:8080/health` against the local-dev
compose without going through TLS. **Never publish this listener to a
non-loopback peer.** Two ways this regresses silently:

- A port-forward (e.g. `ssh -L 8080:127.0.0.1:8080`) that lands the dev
  port on a public-IP workstation, then a colleague hits it over the
  internet because the firewall is permissive. The `127.0.0.1/32` entry
  in the default trust set means Caddy's peer view (still loopback from
  Caddy's perspective on the dev box) is trusted, so any
  `X-Forwarded-For` the colleague's curl sends is honoured — per-IP
  rate-limit keys can be forged at will from that path.
- A docker compose override that binds `:8080` on `0.0.0.0` instead of
  `127.0.0.1` on a developer machine on a shared LAN.

If a developer genuinely needs a non-loopback smoke path on the local-dev
stack — vanishingly rare; the staging stack is for that — copy the
strip block from the wildcard vhost into the `:8080` site so the
defence-in-depth is in place even on dev:

```caddy
:8080 {
  import security-headers.caddy
  request_header -True-Client-IP
  request_header -X-Real-IP
  request_header -X-Forwarded-For
  reverse_proxy app:8080
}
```

Staging (`Caddyfile.stg`) drops the `:8080` block entirely — staging
traffic only reaches the app via 80/443, and the wildcard pattern is
replaced by explicit `$STG_TENANT_HOSTS` entries. Do not port the dev
`:8080` block into a production-shaped Caddyfile.

### 3. Chained reverse proxies (CDN / WAF in front of Caddy)

The current threat model assumes a single proxy hop (Caddy → app). Once a
CDN or WAF (Cloudflare, Fastly, AWS WAF, etc.) is added in front of Caddy,
the trust topology changes in three ways the operator MUST account for:

1. **Caddy becomes the trusted proxy from the CDN's point of view.** The
   CDN, not the original client, is now the immediate TCP peer of Caddy.
   The app's trust gate (`TRUSTED_PROXY_CIDRS`) continues to apply to the
   Caddy→app hop; nothing about the wrapper changes.
2. **The current edge strip wipes any CDN-supplied client IP.** The
   `request_header -True-Client-IP`, `-X-Real-IP`, and `-X-Forwarded-For`
   lines on the tenant vhost are unconditional — they fire regardless of
   the CDN's signing/validation, because Caddy cannot tell a forged
   header from a CDN-supplied one without explicit configuration. This
   is the safe default: until the CDN's identity-forwarding header is
   validated, treating every IP header as untrusted is correct.
3. **The per-IP rate limit collapses to per-CDN-edge granularity.** With
   the headers stripped, `r.RemoteAddr` is whatever Caddy sees, which is
   the CDN edge node's IP. CDNs coalesce thousands of clients through a
   handful of edge IPs, so the 100/min/IP cap on `GET /c/{slug}` becomes
   100/min/edge — orders of magnitude looser. This is *safe* (no bypass,
   just coarser) but not what an operator who pays for a CDN wants.

**To accept a CDN-provided client IP behind Caddy**, the operator must
either:

- Validate the CDN's identity header against the CDN's published edge
  ranges and translate it into `X-Forwarded-For` BEFORE the strip lines
  fire — concrete shape for Cloudflare:

  ```caddy
  *.crm.<stg-domain> {
    import security-headers.caddy

    @from_cdn remote_ip <CDN_EDGE_CIDR_1> <CDN_EDGE_CIDR_2> ...
    handle @from_cdn {
      request_header X-Forwarded-For {http.request.header.CF-Connecting-IP}
    }

    # existing strip block — runs on every request, including the
    # @from_cdn branch above. Because the rewrite ran first, the value
    # forwarded to the app is now the CDN-supplied client IP rather
    # than the raw CF-Connecting-IP header.
    request_header -True-Client-IP
    request_header -X-Real-IP
    # NOTE: do NOT also strip X-Forwarded-For here if the rewrite above
    # populated it — drop the third line in this branch, or move the
    # rewrite to happen AFTER the strip.

    reverse_proxy app:8080
  }
  ```

  (The exact ordering matters; reverse the strip ↔ rewrite order and
  the CDN value is wiped before the app sees it. Pair with the
  Caddyfile `trusted_proxies static` directive when Caddy v2.7+ ships
  with first-class support in your build.)

- Or use Caddy's built-in `trusted_proxies` directive (Caddy v2.7+) once
  it is enabled in this deployment. That moves the trust gate from the
  Go side to the Caddy side; the Go-side wrapper still runs as
  belt-and-braces.

Until that wiring is in place, the per-IP rate limit keys off Caddy's
peer view of the request (CDN edge IP), which is the safe-default
behaviour described above. Document on the change ticket which CIDR set
was used for the CDN's identity header and link to the CDN's published
edge ranges so a future operator can re-verify when those ranges rotate.

**Cross-references.** When the operator change above lands, also update:

- `internal/adapter/httpapi/trusted_realip.go` doc-comment if the
  Caddy→app peer set changes (it usually does not — the CDN sits in
  front of Caddy, not between Caddy and the app).
- `deploy/caddy/Caddyfile.stg` (or whichever Caddyfile is mounted) with
  the rewrite block and an inline comment naming the CDN.
- This runbook section, with the resolved CDN's edge-range refresh
  cadence so the next on-call knows when to re-check.

## Provisioning a fresh staging VPS (target: < 1h)

Assumes Debian 12 / Ubuntu 24.04 with a public IP and root SSH from a bastion.

Before starting, decide which tenant FQDNs the staging stack will host (Fase 0
defaults are `acme.crm.<base>` and `globex.crm.<base>`) and create A records
for each one pointing at the VPS public IP. Caddy uses Let's Encrypt's HTTP-01
challenge to issue certs and that requires the DNS already be in place; a
missing or wrong record means 443 silently never opens. Verify on a
workstation:

```bash
STG_HOST_IP="REPLACE_WITH_VPS_IP"
for host in acme.crm.REPLACE_WITH_BASE globex.crm.REPLACE_WITH_BASE; do
  got=$(dig +short "${host}" A | tail -n1)
  if [ "${got}" = "${STG_HOST_IP}" ]; then
    echo "ok ${host} → ${got}"
  else
    echo "MISMATCH ${host} → ${got:-empty}"
  fi
done
```

### 1. Base packages

Docker publishes separate apt repositories for Debian and Ubuntu — the URL
path is `linux/debian` on Debian and `linux/ubuntu` on Ubuntu. Set both
variables from `/etc/os-release` so the same script works on either distro
and fails fast if the host is something else.

```bash
apt-get update
apt-get install -y ca-certificates curl gnupg ufw fail2ban
install -m 0755 -d /etc/apt/keyrings
. /etc/os-release
case "$ID" in
  debian|ubuntu) DOCKER_DISTRO="$ID" ;;
  *)
    echo "unsupported distro: $ID — Docker repo only ships debian and ubuntu" >&2
    exit 1
    ;;
esac
curl -fsSL "https://download.docker.com/linux/${DOCKER_DISTRO}/gpg" | \
  gpg --dearmor -o /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
  https://download.docker.com/linux/${DOCKER_DISTRO} ${VERSION_CODENAME} stable" \
  > /etc/apt/sources.list.d/docker.list
apt-get update
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker
```

Install cosign (>= v2.4) — required by `stg-deploy.sh` to verify the cosign
keyless signature on every image before `compose pull` (ADR 0084 / SIN-62247).
A missing or out-of-date binary is a hard failure of the deploy gate, not a
warning.

We **bootstrap by self-verify** rather than by a SHA-256 pin: the cosign
release artifacts are themselves cosign-signed by the Sigstore release
identity, so we install the binary, then immediately use it to verify its
own bytes against the signature published next to the download. This keeps
the trust anchor in Sigstore (transparency log + Fulcio cert) rather than in
a SHA-256 hash that would itself need an out-of-band trust anchor.

```bash
COSIGN_VERSION="2.4.1"
BASE="https://github.com/sigstore/cosign/releases/download/v${COSIGN_VERSION}"

# 1. Pull the binary and the two adjacent verification files.
curl -fsSL "${BASE}/cosign-linux-amd64"     -o /tmp/cosign
curl -fsSL "${BASE}/cosign-linux-amd64.sig" -o /tmp/cosign.sig
curl -fsSL "${BASE}/cosign-linux-amd64.pem" -o /tmp/cosign.pem

# 2. Install with executable bit so step 3 can run it.
install -m 0755 /tmp/cosign /usr/local/bin/cosign

# 3. Self-verify: run the just-installed cosign against its own bytes,
#    binding to the Sigstore release identity. A wrong binary or wrong
#    signature aborts here with non-zero exit.
cosign verify-blob \
  --certificate /tmp/cosign.pem \
  --signature   /tmp/cosign.sig \
  --certificate-identity-regexp '^https://github\.com/sigstore/cosign/' \
  --certificate-oidc-issuer     'https://token.actions.githubusercontent.com' \
  /tmp/cosign

# 4. Sanity print + cleanup.
cosign version
rm -f /tmp/cosign /tmp/cosign.sig /tmp/cosign.pem
```

If `cosign verify-blob` fails, **do not proceed** — the binary on disk is
either the wrong release line, a different file than the one Sigstore
signed, or a tampered copy. Re-pull and re-verify; never `chmod +x` a
binary whose self-verify failed.

Bumping `COSIGN_VERSION` here MUST also bump `COSIGN_VERSION` in
`.github/workflows/cd-stg.yml` so the signer and verifier track the same
release line.

Quick troubleshooting if `apt-get install docker-ce` says
`Package docker-ce is not available`:

- `cat /etc/apt/sources.list.d/docker.list` — confirm the URL contains
  `linux/debian` on Debian or `linux/ubuntu` on Ubuntu (matching `$ID` above).
- `cat /etc/os-release | grep -E '^(ID|VERSION_CODENAME)='` — confirm the
  codename is one Docker actually ships (Debian: bullseye/bookworm/trixie,
  Ubuntu: focal/jammy/noble).
- Re-run `apt-get update` and watch the output for any `404 Not Found` lines
  pointing at `download.docker.com` — those mean the URL is wrong for the host.

### 2. Firewall and unattended upgrades

```bash
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw enable
apt-get install -y unattended-upgrades
dpkg-reconfigure -plow unattended-upgrades
```

### 3. Deploy user with constrained SSH

```bash
useradd --create-home --shell /bin/bash crm-deploy
usermod -aG docker crm-deploy
install -d -o crm-deploy -g crm-deploy -m 0700 /home/crm-deploy/.ssh
```

Generate the CD SSH keypair on a workstation (NOT on the VPS), keep the
private half off the host, and install the public half with a strict
`command=` constraint so the key cannot do anything except invoke the
deploy script:

```bash
# on a workstation (or vault host)
ssh-keygen -t ed25519 -C "github-actions cd-stg" -f cd-stg-ed25519 -N ""

# Print the entire pubkey on a single line — copy this whole line to the
# clipboard. It already starts with `ssh-ed25519 AAAA…` and ends with the
# comment `github-actions cd-stg`; do NOT add `ssh-ed25519 ` again on the VPS.
cat cd-stg-ed25519.pub
```

Then on the VPS, paste the entire pubkey into a single shell variable and
let the heredoc interpolate it. Using a variable instead of a literal
placeholder prevents the common footgun of half-replacing the placeholder
and ending up with `ssh-ed25519 AAAA…ssh-ed25519 AAAA<real-key>`, which sshd
silently rejects.

```bash
# Replace REPLACE_… with the EXACT contents of cd-stg-ed25519.pub from the
# workstation — one line, starts with `ssh-ed25519 `, ends with the comment.
PUBKEY="REPLACE_WITH_ENTIRE_LINE_FROM_cd-stg-ed25519.pub"
cat > /home/crm-deploy/.ssh/authorized_keys <<AUTH
command="/opt/crm/stg/bin/deploy.sh",no-pty,no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-user-rc ${PUBKEY}
AUTH
chown crm-deploy:crm-deploy /home/crm-deploy/.ssh/authorized_keys
chmod 600 /home/crm-deploy/.ssh/authorized_keys

# Sanity: file should have exactly one line, no `AAAA…` literal, and exactly
# one occurrence of `ssh-ed25519 `:
test "$(grep -c 'ssh-ed25519 ' /home/crm-deploy/.ssh/authorized_keys)" = "1" \
  || { echo "authorized_keys malformed: pub key duplicated or placeholder kept"; exit 1; }
```

The private half goes into the GitHub repo's `STG_SSH_KEY` secret. Once the
constraint is in place the key cannot start a shell, open a tunnel, or run
arbitrary commands — only `/opt/crm/stg/bin/deploy.sh` with the two-token
remote command `<verb> <image-ref>`. The script enforces the contract:
`<verb>` MUST be one of `deploy` or `migrate-up` (SIN-63332) and the
image-ref MUST match `ghcr.io/pericles-luz/crm@sha256:<64 hex>`. Anything
else aborts before any docker/migrate command runs.

### 4. Stack layout on the VPS

This step lays down `/opt/crm/stg/` on the VPS itself. Run it BEFORE the first
deploy in §5 — `compose.stg.yml` and the deploy wrapper need to be present
before `/opt/crm/stg/bin/deploy.sh` can be invoked.

The repo is private, so `raw.githubusercontent.com` cannot serve the two
artifacts anonymously. Push them from a workstation that already has the repo
cloned (the same workstation you used in §3 to generate the CD SSH keypair):

```bash
# On the workstation, in the cloned `crm` repo root.
# Replace REPLACE_STG_HOST with the staging VPS hostname or IP.
STG_HOST="REPLACE_STG_HOST"
scp deploy/compose/compose.stg.yml deploy/scripts/stg-deploy.sh \
    "root@${STG_HOST}:/tmp/"
# Caddy reads its config from /etc/caddy/, mounted from /opt/crm/stg/caddy/.
# Send the three files Caddy + Unbound need at startup:
#   - Caddyfile.stg, security-headers.caddy        — Caddy
#   - unbound.conf                                 — Unbound sidecar (SIN-62332)
scp deploy/caddy/Caddyfile.stg deploy/caddy/security-headers.caddy \
    infra/caddy/unbound.conf \
    "root@${STG_HOST}:/tmp/"
```

Back on the VPS, lay out the stack directory and install all four files. The
operator running this block must be `root` (or in a sudo session) — the
`crm-deploy` account exists but has no shell.

```bash
# Sanity check: confirm scp landed everything in /tmp.
for f in compose.stg.yml stg-deploy.sh Caddyfile.stg security-headers.caddy; do
  test -s "/tmp/${f}" || { echo "missing /tmp/${f}"; exit 1; }
done

# Lay out the stack directory and install the four files into it.
install -d -o crm-deploy -g crm-deploy -m 0750 \
  /opt/crm/stg /opt/crm/stg/bin /opt/crm/stg/caddy
install -o crm-deploy -g crm-deploy -m 0640 \
  /tmp/compose.stg.yml /opt/crm/stg/compose.stg.yml
install -o root -g crm-deploy -m 0750 \
  /tmp/stg-deploy.sh /opt/crm/stg/bin/deploy.sh
install -o crm-deploy -g crm-deploy -m 0640 \
  /tmp/Caddyfile.stg /opt/crm/stg/caddy/Caddyfile.stg
install -o crm-deploy -g crm-deploy -m 0640 \
  /tmp/security-headers.caddy /opt/crm/stg/caddy/security-headers.caddy

# Empty secrets file with the right ownership; you fill it in below.
install -o crm-deploy -g crm-deploy -m 0640 /dev/null /opt/crm/stg/.env.stg
```

If you ever bump `compose.stg.yml`, `stg-deploy.sh`, or any file under
`deploy/caddy/` on `main`, repeat the same `scp` + `install` flow from a
workstation — the CD pipeline only pushes the application image, not these
on-host artifacts. Automating that sync is tracked as a follow-up; until
then it is operator-driven.

Generate the two infra passwords. They land in `DATABASE_URL` and the MinIO
admin credential, so they MUST be alphanumeric (no `@`, `:`, `/`, `?` —
those break URL parsing in `postgres://user:pass@host/...`). 256 bits of hex
is a safe, copy-paste-friendly default:

```bash
openssl rand -hex 32   # POSTGRES_PASSWORD
openssl rand -hex 32   # MINIO_ROOT_PASSWORD
```

Run each line once, store the outputs in your password manager (1Password,
Bitwarden, etc.) before pasting into `.env.stg`. Losing them later means
recreating the volumes from scratch.

Fill `/opt/crm/stg/.env.stg`. Anything in `REPLACE_…` is a placeholder you
must overwrite — do NOT keep the angle-bracket-style `<digest>` form, bash
parses `<` as input redirection and the line will fail with
`syntax error near unexpected token 'newline'`.

```dotenv
POSTGRES_DB=crm
POSTGRES_USER=crm
POSTGRES_PASSWORD=REPLACE_WITH_HEX_FROM_OPENSSL_RAND
MINIO_ROOT_USER=crm-admin
MINIO_ROOT_PASSWORD=REPLACE_WITH_HEX_FROM_OPENSSL_RAND
# Fase 6 hardening (SIN-63218): default is 1 year (HSTS preload-list minimum).
# Override to a smaller value only during a brand-new host's TLS soak.
HSTS_MAX_AGE=31536000
# Let's Encrypt account contact for cert issuance / expiry warnings. MUST be
# a real RFC 5322 address with a valid TLD — Let's Encrypt and ZeroSSL both
# reject anything else with HTTP 400 invalidContact ("Domain name contains
# an invalid character") and Caddy retries forever with no certs ever issued.
# `name@example.com` is fine; `name@REPLACE_…` is not.
ACME_EMAIL=REPLACE_WITH_REAL_OPS_EMAIL
# Comma-separated list of tenant FQDNs Caddy provisions certs for. Order
# does not matter; every entry must already have an A record pointing at
# the VPS public IP (verify with the dig loop above).
STG_TENANT_HOSTS=acme.crm.REPLACE_WITH_BASE, globex.crm.REPLACE_WITH_BASE
# APP_IMAGE is rewritten by the deploy wrapper on every push. Bootstrap with
# the digest you discover in §5 below — full ref like
# ghcr.io/pericles-luz/crm@sha256:6b8f…f730ba.
APP_IMAGE=REPLACE_WITH_INITIAL_DIGEST_REF
```

### 4b. GHCR pull credentials for the deploy user

GHCR inherits visibility from the source repository, so `crm` images are
private by default. The `crm-deploy` user must be authenticated against
`ghcr.io` BEFORE the first deploy or `docker compose pull` returns
`unauthorized`. There are two acceptable paths — pick one and stick with it.

#### Path A — classic PAT with `read:packages` on the VPS (non-public stg)

GitHub fine-grained PATs do NOT support package access for **user-owned**
packages — the `Packages` permission only appears for organization-owned
ones. For `ghcr.io/pericles-luz/crm`, use a classic PAT with the single
`read:packages` scope (which is itself the smallest scope GitHub exposes for
this use case).

1. On the workstation, open
   `https://github.com/settings/tokens/new?description=ghcr-stg-pull&scopes=read:packages`.
   That URL pre-selects the only required scope; do NOT enable any other
   checkbox. Set the expiry to whatever your rotation policy allows (90 days
   is a sensible default).
2. Copy the token (it starts with `ghp_…`) and run on the VPS:
   ```bash
   GHCR_USER="pericles-luz"
   GHCR_TOKEN="REPLACE_WITH_CLASSIC_PAT"
   sudo -u crm-deploy bash -c "echo '${GHCR_TOKEN}' | docker login ghcr.io -u '${GHCR_USER}' --password-stdin"
   ```
   That writes `~crm-deploy/.docker/config.json` with the encoded
   credential. Subsequent `docker compose pull` runs as `crm-deploy` reuse
   the same file silently.
3. Rotation: generate a new PAT and re-run the `docker login` line — old
   tokens are superseded in `config.json` automatically. Revoke the old PAT
   in the GitHub UI once the new one is in place.

If `crm` ever moves under an organization, switch to a fine-grained PAT
scoped to that org with `Packages: Read-only` and `crm` selected — that
form does work for org packages.

#### Path B — make the GHCR package public

If staging-image visibility is acceptable (no embedded secrets, no
proprietary code beyond what is already inferred from the public
distroless+Go binary): visit
`https://github.com/users/pericles-luz/packages/container/crm/settings`,
scroll to `Danger zone → Change visibility`, switch to `Public`. After that
no `docker login` is needed on the VPS — anonymous pulls succeed.

The CD workflow does not care which path you picked; it pushes with
`secrets.GITHUB_TOKEN` either way.

### 5. First boot

The CD pipeline only takes over once the VPS already runs at least one
working deploy. Until that bootstrap deploy lands, find the digest of an
image already pushed to GHCR and feed it into `deploy.sh` manually.

#### 5a. Find an image digest in GHCR

The `cd-stg` workflow has built and pushed images for every push to `main`
since SIN-62215 merged, even when the SSH step failed (build/push happens
before SSH). Pick whichever digest you want online first:

- **GitHub UI** — open `https://github.com/users/pericles-luz/packages/container/package/crm`,
  click into the version row that matches the SHA you want, and copy the
  `sha256:…` digest from the page header.
- **`gh` CLI on a workstation** — `gh run view <RUN_ID> --repo pericles-luz/crm --log`
  on a recent `cd-stg` run, then grep the `build & push image` block for
  `pushing manifest for ghcr.io/pericles-luz/crm:…@sha256:…` — the digest
  immediately after `@` is what you want.

#### 5b. Run the first deploy

Pin the discovered digest to a shell variable to avoid re-typing it (and to
sidestep the `<digest>` placeholder trap):

```bash
DIGEST="sha256:REPLACE_WITH_64_HEX_DIGEST"
APP_IMAGE_REF="ghcr.io/pericles-luz/crm@${DIGEST}"

# 1. Make .env.stg agree with what we are about to deploy.
sudo sed -i "s|^APP_IMAGE=.*|APP_IMAGE=${APP_IMAGE_REF}|" /opt/crm/stg/.env.stg
sudo grep '^APP_IMAGE=' /opt/crm/stg/.env.stg   # sanity

# 2. Run the deploy wrapper as the constrained user (NOT root):
sudo -u crm-deploy /opt/crm/stg/bin/deploy.sh deploy "${APP_IMAGE_REF}"

# 3. Internal smoke check first — confirms the app/caddy/network plumbing
#    works without depending on Let's Encrypt:
sudo -u crm-deploy docker exec crm-stg-caddy-1 wget -qO- http://app:8080/health

# 4. External smoke check. The first hit on each tenant FQDN triggers
#    Let's Encrypt issuance, which usually takes 5–30s. If the first curl
#    returns 525/timeout, retry once after 30 s. (Replace the URL with the
#    same value you will set in the STG_SMOKE_URL secret.)
STG_SMOKE_URL="https://acme.crm.REPLACE_WITH_STG_DOMAIN"
curl -fsS "${STG_SMOKE_URL}/health"
```

If both smoke checks return JSON shaped like `{"status":"ok","commit_sha":"<sha>"}`
you are done; subsequent deploys are fully automated by the `cd-stg`
workflow once you finish §6 and populate the GitHub Actions secrets.
SIN-63146 added `commit_sha` to `/health`; the SHA is `unknown` for any
image built without `--build-arg COMMIT_SHA=…` (e.g. a local `docker
build .`), and equals `github.event.workflow_run.head_sha` for every
image pushed by `cd-stg.yml`. The `cd-stg` smoke step now compares the
two and fails red if they disagree — this is the tripwire for a stale
`docker compose pull` and matches the SIN-63143 diagnosis.

If the external check times out on `port 443` indefinitely, in that order:

1. `sudo -u crm-deploy docker logs crm-stg-caddy-1 --tail 50` — Caddy logs
   the Let's Encrypt failure inline; common offenders are:
   - **`invalidContact: contact email has invalid domain`** — `ACME_EMAIL`
     in `.env.stg` still has a placeholder or otherwise invalid TLD (`_`,
     `REPLACE_…`, etc). Fix the value, then `compose up -d --force-recreate
     caddy` (env vars only re-read on container creation, not on `restart`).
   - Missing or wrong DNS A records.
   - UFW blocking 80 (LE needs **both** 80 and 443 — 80 for the HTTP-01
     challenge, 443 for the eventual cert handshake).
2. `dig +short <fqdn>` from a workstation — confirm DNS resolves to the VPS.
3. `sudo ufw status` on the VPS — confirm 80 and 443 are allowed.

### 5c. Apply database migrations

**Automatic path (SIN-63332).** On every `cd-stg` run after `main`
goes green, the `migrate-up` step (between the `/health` and `/login`
smokes) SSHes `migrate-up <APP_IMAGE_REF>` to
`/opt/crm/stg/bin/deploy.sh`. The script extracts `/migrations` from the
just-deployed image via `docker cp`, then runs a one-shot
`migrate/migrate:v4.17.1` sidecar pinned by manifest-list digest inside
the staging compose network. golang-migrate is idempotent — re-running
against an already-migrated DB is a no-op — so the step is safe to
re-trigger after a transient failure. The DSN parts are read from
`/opt/crm/stg/.env.stg` (`POSTGRES_USER` / `POSTGRES_PASSWORD` /
`POSTGRES_DB`); they are NOT inlined in the workflow and the GitHub
runner never sees plaintext DB credentials. See SIN-63332 and the
`migrate-up` step in `.github/workflows/cd-stg.yml`.

Migrations no longer have to be `scp`-ed to the VPS by hand — the
Dockerfile's `crm-server` stage now ships `/migrations` alongside the
binary (SIN-63332), and `deploy.sh migrate-up` reads them straight from
the just-deployed image. The manual procedure below is still documented
as **break-glass** for the cases where the automatic step cannot be
used (initial bring-up, recovery, or `cd-stg` itself is broken).

**Break-glass: manual `migrate up` from the VPS.** The local `make
migrate-up` (Makefile) reads `compose/.env` and joins the dev compose
network. On the VPS, Postgres is in the staging compose stack
(`crm-stg-postgres-1`) and credentials live in `/opt/crm/stg/.env.stg`,
so manual migrations need the same one-shot `migrate/migrate` container
the script uses, scoped to the staging compose project. The image is
pinned by SHA256 digest — never `:latest` — following the same policy as
the infra images (see "Bumping infra image digests").

Manual mode also requires the migrations bytes on the VPS filesystem.
Push them once during provisioning and re-push after any migration is
added (parallel to the `compose.stg.yml` scp flow in §4):

```bash
# On the workstation, in the cloned crm repo root.
STG_HOST="REPLACE_STG_HOST"
scp -r migrations "root@${STG_HOST}:/tmp/migrations"
```

Back on the VPS, install them under the staging stack root:

```bash
sudo install -d -o crm-deploy -g crm-deploy -m 0750 /opt/crm/stg/migrations
sudo cp -a /tmp/migrations/. /opt/crm/stg/migrations/
sudo chown -R crm-deploy:crm-deploy /opt/crm/stg/migrations
```

Resolve the `migrate/migrate` digest the same way as any other infra
image (see "Bumping infra image digests" for the two flows). The Makefile
pins `migrate/migrate:v4.17.1`; the snippet below uses that tag — replace
the trailing `@sha256:…` with the digest you just resolved:

```bash
# Resolve once and pin:
docker buildx imagetools inspect migrate/migrate:v4.17.1 \
  --format '{{ .Manifest.Digest }}'
# or via curl + the Docker Hub registry API as documented in
# "Bumping infra image digests".
MIGRATE_IMAGE="migrate/migrate:v4.17.1@sha256:REPLACE_WITH_RESOLVED_DIGEST"
```

Run the migrations against the staging Postgres:

```bash
COMPOSE_ARGS="--env-file /opt/crm/stg/.env.stg -f /opt/crm/stg/compose.stg.yml"

# Resolve the compose project network so the migrate container can reach
# the in-stack `postgres` hostname. Compose name = project name + _default.
NETWORK="$(sudo -u crm-deploy docker compose ${COMPOSE_ARGS} ps -q postgres \
  | xargs -r sudo docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' \
  | head -n1)"
test -n "${NETWORK}" || { echo "could not resolve compose network"; exit 1; }

# Read DSN parts straight out of the env file (no shell-source — keeps
# secrets out of this terminal session's env).
read_env() { sudo grep -E "^${1}=" /opt/crm/stg/.env.stg | tail -n1 | cut -d= -f2-; }
POSTGRES_USER_VAL="$(read_env POSTGRES_USER)"
POSTGRES_PASSWORD_VAL="$(read_env POSTGRES_PASSWORD)"
POSTGRES_DB_VAL="$(read_env POSTGRES_DB)"

sudo -u crm-deploy docker run --rm \
  --network "${NETWORK}" \
  -v /opt/crm/stg/migrations:/migrations:ro \
  "${MIGRATE_IMAGE}" \
  -path /migrations \
  -database "postgres://${POSTGRES_USER_VAL}:${POSTGRES_PASSWORD_VAL}@postgres:5432/${POSTGRES_DB_VAL}?sslmode=disable" \
  up
```

Verify the migration table now lists every file under `/opt/crm/stg/migrations`:

```bash
sudo -u crm-deploy docker compose ${COMPOSE_ARGS} exec -T postgres \
  psql -v ON_ERROR_STOP=1 -U "${POSTGRES_USER_VAL}" -d "${POSTGRES_DB_VAL}" \
  -c "SELECT version, dirty FROM schema_migrations ORDER BY version DESC LIMIT 5;"
```

**Failure mode if you skip this step.** The app boots, `/health` returns
`{"status":"ok","commit_sha":"<sha>"}` (it is static and does not touch
the DB by design), and the `cd-stg` `/health` smoke stays green — but
any endpoint that reads from a missing table returns `500`. A `/health`
200 is therefore necessary but not sufficient for "deploy is healthy".
The automatic `migrate-up` step (SIN-63332) is the **prevention** layer
that closes this gap; the `/login` smoke gate (SIN-63270) is the
**detection** layer that catches a real regression even when migrations
are up to date. Both stay. If you ever need to verify the DB schema by
hand — e.g. after a break-glass restore — two options that do not need
an authenticated session:

```bash
# Option A — count rows in a known table from inside the postgres container:
sudo -u crm-deploy docker compose ${COMPOSE_ARGS} exec -T postgres \
  psql -v ON_ERROR_STOP=1 -U "${POSTGRES_USER_VAL}" -d "${POSTGRES_DB_VAL}" \
  -c "SELECT count(*) FROM tenants;"

# Option B — hit a tenant endpoint and check that the response body
# isn't an HTTP 500 SQL error page. Replace the host with a real tenant
# FQDN from .env.stg (STG_TENANT_HOSTS):
curl -fsS "https://acme.crm.REPLACE_WITH_STG_DOMAIN/login" >/dev/null \
  && echo "tenant DB path OK"
```

### 5d. Apply staging seed (with tenant FQDN substitution)

`migrations/seed/stg.sql` defines two tenants (`acme`, `globex`) and one
agent user per tenant. Until SIN-63146 the file hard-coded
`acme.crm.local`/`globex.crm.local`; it now templates the FQDN suffix
through the `:base_domain` psql variable so the same file seeds dev
(`crm.local`) and staging (the real VPS suffix) without any sed/awk
munging.

The `make seed-stg` target wires the default `${STG_BASE_DOMAIN:-crm.local}`
so the local dev flow is unchanged. On the VPS, pass the real suffix
through `STG_BASE_DOMAIN`. The tenant rows end up as `acme.${base_domain}`
and `globex.${base_domain}`; pick the value that matches the FQDNs
already published in DNS and in `STG_TENANT_HOSTS`:

```bash
COMPOSE_ARGS="--env-file /opt/crm/stg/.env.stg -f /opt/crm/stg/compose.stg.yml"

# Use the same suffix every other DNS / Caddy / TLS layer expects.
# Example: tenants are acme.crm.someu.com.br and globex.crm.someu.com.br
#          ⇒ STG_BASE_DOMAIN="crm.someu.com.br"
# Tripwire: must NOT end in .local — that's the dev default, refusing
# it here is the only line of defence against accidentally seeding
# staging with dev hosts.
STG_BASE_DOMAIN="REPLACE_WITH_STG_TENANT_SUFFIX"

read_env() { sudo grep -E "^${1}=" /opt/crm/stg/.env.stg | tail -n1 | cut -d= -f2-; }
POSTGRES_USER_VAL="$(read_env POSTGRES_USER)"
POSTGRES_DB_VAL="$(read_env POSTGRES_DB)"

sudo -u crm-deploy docker compose ${COMPOSE_ARGS} exec -T postgres \
  psql -v ON_ERROR_STOP=1 \
       -v base_domain="${STG_BASE_DOMAIN}" \
       -U "${POSTGRES_USER_VAL}" -d "${POSTGRES_DB_VAL}" \
  < /opt/crm/stg/migrations/seed/stg.sql
```

The repo ships `scripts/stg-apply-seed.sh` as a one-liner wrapper around
the same command, with the empty-string and `.local` tripwires baked in.
Push it once with the other deploy artifacts (§4) and call it on every
re-seed:

```bash
sudo install -o root -g crm-deploy -m 0750 \
  /tmp/stg-apply-seed.sh /opt/crm/stg/bin/stg-apply-seed.sh

STG_BASE_DOMAIN="REPLACE_WITH_STG_TENANT_SUFFIX" \
  /opt/crm/stg/bin/stg-apply-seed.sh
```

**Validation.** Confirm the tenant rows have the expected hosts:

```bash
sudo -u crm-deploy docker compose ${COMPOSE_ARGS} exec -T postgres \
  psql -v ON_ERROR_STOP=1 -U "${POSTGRES_USER_VAL}" -d "${POSTGRES_DB_VAL}" \
  -c "SELECT name, host FROM tenants ORDER BY name;"
```

The output must match `acme.${STG_BASE_DOMAIN}` and
`globex.${STG_BASE_DOMAIN}` — anything still showing `.local` means the
substitution did not run (likely the operator forgot the `-v base_domain=`
flag and Postgres errored with `psql:…: ERROR: syntax error at or near
":"`, in which case `ON_ERROR_STOP=1` already rolled back the
transaction). Re-run with the correct `STG_BASE_DOMAIN` and re-validate.

### 6. Capturing the staging host key

The runner verifies the VPS host key via `STG_HOST_KEY` to avoid TOFU. Capture
it once during provisioning:

```bash
STG_HOST="REPLACE_STG_HOST"
ssh-keyscan -t ed25519 "${STG_HOST}" | tee stg.host_key
```

Paste the output of that file into the `STG_HOST_KEY` GitHub Actions secret.

## Reading logs

`docker compose` parses `compose.stg.yml` even for read-only operations like
`logs`, and `compose.stg.yml` uses `${VAR:?…}` placeholders, so every
invocation needs `--env-file /opt/crm/stg/.env.stg` — otherwise compose errors
with `required variable … is missing a value` before it ever reads container
state.

```bash
COMPOSE_ARGS="--env-file /opt/crm/stg/.env.stg -f /opt/crm/stg/compose.stg.yml"

# All services, follow:
sudo -u crm-deploy docker compose ${COMPOSE_ARGS} logs -f --tail=200
# Single service:
sudo -u crm-deploy docker compose ${COMPOSE_ARGS} logs -f --tail=500 app
# What is currently deployed:
sudo -u crm-deploy grep '^APP_IMAGE=' /opt/crm/stg/.env.stg
sudo -u crm-deploy cat /opt/crm/stg/.last-image  # what was running before this deploy
```

## Manual rollback (smoke check went red)

The deploy wrapper records the previous `APP_IMAGE` in `/opt/crm/stg/.last-image`
just before swapping. To revert:

```bash
STG_SMOKE_URL="https://acme.crm.REPLACE_WITH_STG_DOMAIN"
prev="$(cat /opt/crm/stg/.last-image)"
sudo -u crm-deploy /opt/crm/stg/bin/deploy.sh deploy "${prev}"
curl -fsS "${STG_SMOKE_URL}/health"
```

If the rollback also fails, `compose down && compose up` is the next escalation;
beyond that, file a ticket and page the on-call.

## GitHub Actions secrets used by `cd-stg`

| Secret             | Purpose                                                                          |
| ------------------ | -------------------------------------------------------------------------------- |
| `GITHUB_TOKEN`     | Auto-issued by Actions; `packages: write` is enough to push to `ghcr.io`.        |
| `STG_SSH_KEY`      | Private half of the CD ed25519 keypair (locked by `command=` on the VPS).        |
| `STG_HOST`         | DNS name or IP of the staging VPS.                                               |
| `STG_HOST_KEY`     | Output of `ssh-keyscan -t ed25519 <stg-host>` — populates the runner's known_hosts. |
| `STG_USER`         | Unprivileged deploy user (`crm-deploy` in this runbook).                         |
| `STG_SMOKE_URL`    | Base URL the runner curls — e.g. `https://acme.crm.<stg-domain>`. Used by both the `/health` gate (SIN-63146) and the `/login` gate (SIN-63270). |
| `STG_SEED_AGENT_EMAIL` | Email of the seeded staging tenant agent the `/login` smoke gate posts as — `agent@acme.<stg-domain>`, matching `migrations/seed/stg.sql`. Required by SIN-63270. |
| `STG_SEED_AGENT_PASSWORD` | Password for the seed agent above (`stg-password` for the current seed). Set on the fork; coordinate with Pericles for upstream tier-2 if/when `cd-stg` runs on `pericles-luz/crm`. Required by SIN-63270. |

`GHCR_TOKEN` is not used; the auto-issued `GITHUB_TOKEN` is preferred because
it rotates on every run and is scoped to this repo's packages only.

## Login smoke gate (SIN-63270)

After the `/health` gate clears, `cd-stg` runs a second post-deploy gate
that exercises the authenticated path against the just-deployed staging
URL. The structural reason it exists is PR #104: a 43-commit deploy
shipped a panic-on-valid-creds regression (F10) because no gate in the
pipeline ever called `POST /login` end-to-end. `/health` cannot catch it
— the handler does not touch IAM, the session store, or argon2
verification.

The gate is the deploy-pipeline counterpart of
`internal/adapter/db/postgres/login_seed_e2e_test.go` (SIN-63269): same
three cases, same assertions, but against the running staging stack so
deploy-time regressions (image drift, env-var typo, missing migration)
fail closed.

**Matrix the gate enforces:**

1. `agent@acme.<stg-domain>` + `stg-password` → `302 Found`,
   `Set-Cookie: __Host-sess-tenant`, `Set-Cookie: __Host-csrf`,
   `Location: /hello-tenant`.
2. Same email + wrong password → `401 Unauthorized`.
3. Unknown email + any password → `401 Unauthorized` (same body as
   case 2 — divergence here would be a user-enumeration regression).

**Budget:** the step caps at 1 minute (`timeout-minutes: 1`); inside
that the retry loop runs up to 3 attempts at ~5 s each with a 5 s
backoff between attempts (≤ ~25 s wall clock for the happy path, ≤ 30 s
for two retries). `curl --max-time 5` bounds each individual hit so a
hung handler cannot wedge the job. The gate deliberately does NOT
assert cookie attribute strings (`HttpOnly; Secure; SameSite=Lax`) —
those vary by environment and are covered by other tests.

**Operator checklist when this gate goes red:**

1. Read the workflow log — the first failed case logs `::error::…` plus
   the relevant response body or headers.
2. Mirror the failing case locally:
   ```bash
   STG_BASE="https://acme.crm.<stg-domain>"
   STG_SEED_AGENT_EMAIL="agent@acme.<stg-domain>"
   STG_SEED_AGENT_PASSWORD="stg-password"
   curl -sS -o /tmp/login.body -D /tmp/login.headers -w "%{http_code}\n" \
     -X POST -H "Content-Type: application/x-www-form-urlencoded" \
     --data-urlencode "email=${STG_SEED_AGENT_EMAIL}" \
     --data-urlencode "password=${STG_SEED_AGENT_PASSWORD}" \
     "${STG_BASE}/login"
   ```
3. If the local repro also fails, proceed to `Manual rollback` above —
   the deploy is broken on the authenticated path even though `/health`
   is green.

**Rotating the seed password:** if `stg-password` ever changes in
`migrations/seed/stg.sql`, update `STG_SEED_AGENT_PASSWORD` on the
`ia-dev-sindireceita/crm` fork (and on `pericles-luz/crm` for tier-2)
in the same PR — the seed file and the secret value must match or
case 1 of the gate goes red forever.

## Bumping infra image digests

`compose.stg.yml` pins postgres, caddy, redis, nats, and minio by SHA256 digest.
When you want to take a security or feature update for one of them:

1. Pick the new versioned tag (e.g. `postgres:16.5-alpine3.20`). Floating tags
   such as `:latest`, `:16-alpine`, `:7-alpine` are forbidden — the
   `grep -E ':(latest|alpine)$' deploy/compose/compose.stg.yml` check will
   reject them in CI.
2. Resolve the digest. Either tool below works, no docker daemon required:
   ```bash
   # docker buildx (when local docker is available):
   docker buildx imagetools inspect postgres:16.5-alpine3.20 \
     --format '{{ .Manifest.Digest }}'
   # or via curl + the registry HTTP API:
   tok=$(curl -fsS \
     "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/postgres:pull" \
     | jq -r .token)
   curl -fsSI \
     -H "Authorization: Bearer $tok" \
     -H "Accept: application/vnd.oci.image.index.v1+json" \
     -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
     https://registry-1.docker.io/v2/library/postgres/manifests/16.5-alpine3.20 \
     | awk '/^[Dd]ocker-[Cc]ontent-[Dd]igest:/ { print $2 }'
   ```
3. Replace the line in `compose.stg.yml` with the new `tag@sha256:…` reference,
   keeping the human-readable tag for context.
4. Run the AC #6 check locally:
   ```bash
   grep -nE ':(latest|alpine)$' deploy/compose/compose.stg.yml && exit 1 || true
   ```
5. Open a normal PR. Reviewers see the new digest, the CI gate enforces the
   shape, and the next push to `main` deploys it via `cd-stg`.

## Known limitations (Fase 0 scope)

- **No automatic rollback.** AC #3 of SIN-62215 deliberately keeps rollback
  manual; auto-rollback is parked for Fase 6 or a follow-up issue.
- **Single-tenant smoke check.** The runner curls one URL (`STG_SMOKE_URL`).
  The "two tenants in stg" goal of Fase 0 critério #2 is satisfied by the
  Caddy wildcard host plus a future per-tenant probe (Fase 1 follow-up).
- **No host-level Prometheus / log shipping yet.** PR10/11 of Fase 0 wires
  alerting and observability; the runbook above is what we have until then.
