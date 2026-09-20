<p align="right">
  <strong>English</strong> | <a href="./README.md" aria-label="切换到中文版 README">中文</a>
</p>

<div align="center">

# CC Hub Go

**claude-code-hub-go** — the backend of [claude-code-hub](https://github.com/ding113/claude-code-hub) **rewritten from scratch in Go, shipped as a single binary**

[![Release](https://img.shields.io/github/v/release/fanxcv/claude-code-hub-go?label=release)](https://github.com/fanxcv/claude-code-hub-go/releases)
[![Docker Pulls](https://img.shields.io/docker/pulls/fanxcv/claude-code-hub-go)](https://hub.docker.com/r/fanxcv/claude-code-hub-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](./LICENSE)

</div>

> [!IMPORTANT]
> **This is a fork, not a drop-in replacement for the upstream project.**
> Upstream [ding113/claude-code-hub](https://github.com/ding113/claude-code-hub) is a Node.js (Next.js) full-stack app.
> This repository keeps upstream's **database schema, Redis key space and web UI**, and **rewrites the entire backend in Go**: the data plane (proxy + protocol conversion), the management plane (REST API), the database migrator and page serving all live inside one Go process.
> Upstream is MIT-licensed; copyright belongs to its authors. This fork is MIT-licensed too.

---

## Table of contents

- [1. What it is](#1-what-it-is)
- [2. What changed compared to upstream](#2-what-changed-compared-to-upstream)
- [3. Getting started](#3-getting-started)
- [4. Configuration](#4-configuration)
- [5. Data and migrations (interchangeable with upstream)](#5-data-and-migrations-interchangeable-with-upstream)
- [6. Measured capacity and performance](#6-measured-capacity-and-performance)
- [7. Development](#7-development)
- [8. Tracking upstream](#8-tracking-upstream)
- [9. License and credits](#9-license-and-credits)

---

## 1. What it is

A **self-hosted LLM gateway** for Claude Code / Codex / Gemini and similar clients: it pools multiple upstream providers behind one endpoint and adds routing, protocol conversion, rate limiting, circuit breaking, usage metering and a web console.

It runs as **one process, one port, one image**: a statically linked Go binary with the web UI (Next.js static export, brotli-compressed) embedded — no Node runtime, no separate static file directory.

Endpoints:

| Path | Purpose |
| --- | --- |
| `/v1/messages` | Anthropic protocol (Claude Code clients) |
| `/v1/chat/completions` | OpenAI Chat Completions |
| `/v1/responses` | OpenAI Responses (including the WebSocket variant) |
| `/v1beta/**` | Gemini protocol |
| `/v1/models`, `/v1/_ping` | Model list, liveness |
| `/api/v1/**` | Management REST API (OpenAPI is generated at runtime from the registered route table; see `/api/v1/openapi.json`) |
| `/readyz`, `/api/health` | Readiness (per-dependency: PG / Redis / rule snapshot / draining) and health |
| `/debug/metrics`, `/debug/pprof/**` | Runtime metrics and profiling (**off by default**, loopback-only when enabled) |
| `/**` | Web console (served from the embedded UI bundle) |

## 2. What changed compared to upstream

### 2.1 The backend is a rewrite, not a patch

| Dimension | Upstream (Node.js) | This fork (Go) |
| --- | --- | --- |
| Runtime | Node 22 + Next.js standalone | Single statically linked binary (`CGO_ENABLED=0`) |
| Data plane | TypeScript (`src/app/v1/_lib/proxy/**`) | Go: guard chain → provider selection → dialing → stream gating → streaming pump → terminal settlement |
| Protocol conversion | TypeScript converters | Built into Go (Anthropic ↔ OpenAI Chat ↔ OpenAI Responses ↔ Gemini) |
| Management plane | Next.js route handlers + server actions | Native Go REST (OpenAPI generated at runtime from the registered route table) |
| Page serving | Next SSR + API routes | Static export → brotli → `go:embed`, answered by Go directly |
| DB migrations | `drizzle-kit` / `drizzle-orm` migrator | Built-in Go migrator, **byte-for-byte aligned** with drizzle's journal / hash / statement semantics |
| Rate limiting and sessions | TypeScript constants + Lua | 18 Lua scripts **byte-identical**, verified by golden replay |
| Image size | 333 MB | **45.8 MB** |

### 2.2 Capabilities upstream does not have

**Reliability and correctness**

- **Full decision-chain tracing**: the chain records the candidate pool (`consideredCandidates`), same-priority candidates (`candidatesAtPriority`) and — when prefix affinity short-circuits selection — the providers that passed every hard check but never competed (`survivingCandidates`). Every attempt carries `statusCode`, `errorMessage` and `modelRedirect`, all viewable in the console.
- **Circuit-breaker backoff ladder**: configurable "base + increment × attempt (capped)" so repeated half-open failures push the window out progressively (disabled by default, preserving upstream behaviour).
- **Circuit-log viewer**: per-provider recent failures with the real error messages (redacted), instead of counters with no cause.
- **Protocol-conversion failures are recorded**: conversion fallback is silent upstream; this fork additionally emits a `protocol_conversion_failed` audit entry so "why did this request leave in an unexpected dialect" is answerable.
- **Settlement barrier**: on shutdown the process waits for in-flight requests and pending settlement writes, so rolling restarts cannot leave holes in the ledger (upstream has no such barrier).
- **Self-healing patrol** for rows that never reached a terminal state.
- **Built-in migrations**: drizzle migrations are applied on startup (`AUTO_MIGRATE`), no Node-side migration command required.
- **Request replay**: the client-visible body is retained in two layers (a Redis hot layer plus a PostgreSQL durable layer); a repeat request hits the stored copy and is replayed, so retries and multiple clients cannot be double-billed. Bodies are stored in chunks — no complete body is ever held locally at any instant, and exceeding the budget degrades to no-replay. Key shapes are byte-identical to upstream (`cch:replay:owner|meta|chunks:<replayId>`).
- **WebSocket channel for `/v1/responses`**: client WS text frames go through the same guard chain and forwarding trunk, and each upstream SSE event is turned back into one WS frame; attribution, the draining gate and request logging apply to WS turns as well.

**Performance and resource use**

- **< 1 MiB resident per stream**: an 8 MiB response body is streamed through without being held in memory (upstream holds ~8.57 MiB per stream under the same load).
- **Outbound compression**: br/gzip negotiation on management-plane JSON. For a payload shaped like the real one (50 rows × 54 fields), the in-repo test measures br **269,524 → 11,254 bytes (23.95×)** and gzip **17,585 bytes (15.33×)**; production `/api/v1/usage-logs` measures about **22×**.
- **JIT disabled on database connections**: `/dashboard/overview` went from **100.9–102.5 ms** to **9.1–9.9 ms** (~11×). The planner overestimated the query by 236×, pushing its cost past `jit_above_cost` and paying PostgreSQL JIT compilation.
- **Zero-allocation stream detection**: the request body is scanned at the top level to find `stream` instead of building a full JSON tree (same 50 KiB body: 564,835 → 39,144 B allocated, 9,555 → 47 allocs).
- **Short-TTL stats cache + incremental fetching**: the usage-log page supports `sinceId` deltas, and the stats panel is cached briefly.

**Observability and operations**

- **`/debug/metrics` + pprof**: Go runtime metrics (GC, heap, goroutines) plus CPU/heap/block/mutex profiles — off by default, loopback-only when enabled.
- **Single version source of truth**: `/api/version`, `/api/health` and the UI footer always agree (upstream once reported three different versions from those three places).
- **Layered readiness probe**: `/readyz` names the failing dependency; it returns 503 while draining, and `/v1/_ping` keeps answering.
- **Rate limiting and auth throttling wired into the data plane**, with a rejection envelope identical to upstream (`code`, `limit_type`, `current`, `limit`, …).

**UI and features**

- The usage-log page supports **polling and push (SSE) modes** with a configurable interval (1/2/3/5/10 s).
- **Same-protocol preference**: among equally-prioritised providers, prefer the one that needs **no protocol conversion** (`CCH_SAME_PROTOCOL_WEIGHT_K`, default `2`; set to `1` to disable, which is equivalent to not wiring it). The predicate reuses the very pairing table the guard uses for format filtering, so the preference can never point at a provider that still needs conversion.
- **Session idle gate**: prefix affinity stops being renewed once the same client session has been idle, so affinity cannot pin traffic to a stale (slower or pricier) provider forever.
- UI languages are Simplified Chinese and English; the retired `ja` / `ru` / `zh-TW` prefixes keep **redirect shells**, so old links never 404.

### 2.3 Consistency with upstream, and intentional differences

Alignment is not maintained by eyeballing: it rests on reproducible comparisons — golden samples (byte-recorded upstream responses), Lua golden replays, a protocol conformance matrix (`tests/load/protocol-matrix/`), and a switch comparison (its fixture is not shipped in this repo).

Intentional differences (**these fix upstream defects; they are not regressions**):

- One upstream admin query has an ambiguous `id` column, so its activity stream always returned an empty array; this fork returns the non-empty shape upstream's own types describe (recorded as `node-defect` in the switch matrix, not counted as a failure).
- Upstream emits an empty `content` for an assistant message that has thinking but no text and no tool calls, which strict upstreams reject with 400; this fork carries the thinking text and preserves reasoning pass-back.

## 3. Getting started

### 3.1 Docker Compose (bundled PostgreSQL and Redis)

```bash
git clone https://github.com/fanxcv/claude-code-hub-go.git
cd claude-code-hub-go
cp .env.example .env
# Change at least ADMIN_TOKEN and DB_PASSWORD; adjust APP_PORT if needed
$EDITOR .env
docker compose up -d
```

Open `http://<host>:23000` and sign in with `ADMIN_TOKEN`. Schema is created on first start (`AUTO_MIGRATE=true`).

### 3.2 Single container (existing PostgreSQL and Redis)

```bash
docker run -d --name cchd \
  -p 23000:23000 \
  -e DSN='postgres://user:pass@<pg-host>:5432/claude_code_hub' \
  -e REDIS_URL='redis://<redis-host>:6379' \
  -e ADMIN_TOKEN='<your-admin-token>' \
  -e CCH_EGRESS_PAGES=embed \
  -e TZ=Asia/Shanghai \
  --restart unless-stopped \
  fanxcv/claude-code-hub-go:latest
```

`CCH_EGRESS_PAGES` decides where page requests go: `embed` answers from the built-in UI, `off` serves no pages at all. The program default is `off` (a conservative default: it does not assume page assets exist), but **the official image pins `CCH_EGRESS_PAGES=embed` in its runtime stage** (see `go/deploy/Dockerfile`) and the `docker-compose.yaml` template sets it too — so you do **not** need to set it when starting from the official image or compose; the `-e CCH_EGRESS_PAGES=embed` above only spells out the effective value. Override it with `off` to disable page serving (for example when pages are hosted separately).

### 3.3 Release binaries

Download the archive for your platform from [Releases](https://github.com/fanxcv/claude-code-hub-go/releases) (no runtime to install; UI embedded). **Six platform targets**; every archive contains the binary plus `LICENSE` and `README.md`:

| Platform | Archive | Binary inside |
| --- | --- | --- |
| Linux x86-64 | `cchd-linux-amd64.tar.gz` | `cchd` |
| Linux arm64 | `cchd-linux-arm64.tar.gz` | `cchd` |
| macOS Intel | `cchd-darwin-amd64.tar.gz` | `cchd` |
| macOS Apple Silicon | `cchd-darwin-arm64.tar.gz` | `cchd` |
| Windows x64 | `cchd-windows-amd64.zip` | `cchd.exe` |
| Windows on Arm | `cchd-windows-arm64.zip` | `cchd.exe` |

Dynamic dependencies (measured): on Linux the binary is fully static (no `NEEDED` entries); on macOS it links only the system-provided `libSystem`/`libresolv`/`CoreFoundation`/`Security` (macOS does not allow statically linking libSystem); on Windows it imports only the system-provided `kernel32.dll`.

All six share one `SHA256SUMS`:

```bash
sha256sum -c SHA256SUMS             # Linux
shasum -a 256 -c SHA256SUMS         # macOS (shasum ships with the OS)
Get-FileHash .\cchd-windows-amd64.zip -Algorithm SHA256   # Windows (compare with SHA256SUMS)
```

Linux / macOS:

```bash
tar -xzf cchd-linux-amd64.tar.gz && cd cchd-linux-amd64
chmod +x cchd
DSN='postgres://user:pass@127.0.0.1:5432/claude_code_hub' \
REDIS_URL='redis://127.0.0.1:6379' \
ADMIN_TOKEN='<your-admin-token>' \
PORT=23000 CCH_EGRESS_PAGES=embed ./cchd
```

Windows (PowerShell):

```powershell
Expand-Archive cchd-windows-amd64.zip -DestinationPath .
cd cchd-windows-amd64
$env:DSN='postgres://user:pass@127.0.0.1:5432/claude_code_hub'
$env:REDIS_URL='redis://127.0.0.1:6379'
$env:ADMIN_TOKEN='<your-admin-token>'
$env:PORT='23000'; $env:CCH_EGRESS_PAGES='embed'
.\cchd.exe
```

### 3.4 Building from source

Requirements: Go 1.25+, Bun (or Node 22+ with bun). The UI bundle is produced at **build time** and embedded, so build the UI first, then the binary:

```bash
bun install
bun run build          # static export → brotli → into go/internal/uiapp/assets
cd go
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o cchd ./cmd/cchd
```

Building a container image (use `docker buildx` for multi-arch):

```bash
# after generating the UI bundle as above:
docker build -f go/deploy/Dockerfile -t fanxcv/claude-code-hub-go:local .
```

## 4. Configuration

The full list lives in [`.env.example`](./.env.example). The essentials:

| Variable | Default | Notes |
| --- | --- | --- |
| `DSN` | — | PostgreSQL connection string (**required in production**; without it the whole data plane is absent) |
| `REDIS_URL` | — | Redis connection string (**required in production**; without it the process still starts, with session binding, affinity, replay and rate limiting degraded) |
| `ADMIN_TOKEN` | — | Console and Admin API token (**required in production**; never keep the placeholder) |
| `PORT` | `23000` | Listen port (the image `EXPOSE`s it; probes follow this value) |
| `AUTO_MIGRATE` | `true` | Apply database migrations on startup |
| `ENABLE_RATE_LIMIT` | `true` | Enable rate limiting |
| `SESSION_TTL` | `300` | Session/context cache TTL (seconds) |
| `CCH_EGRESS_PAGES` | `off` (**image pins `embed`**) | Page serving: `embed` answers from the built-in UI; `off` serves no pages. The image and the compose template both set `embed`, so **no manual setting is needed**; set `off` explicitly to disable page serving |
| `CCH_SAME_PROTOCOL_WEIGHT_K` | `2` | Weight multiplier for same-protocol candidates (routing preference); `1` disables it |
| `DB_POOL_MAX`, … | see `.env.example` | Pool budget and per-stage timeouts |
| `GOMEMLIMIT` | — | Go runtime soft memory limit (set it **below** the container `mem_limit`, e.g. `448MiB`, so the runtime GCs before the cgroup cap) |
| `CCH_PPROF_ENABLED` / `CCH_PPROF_ADDR` | `false` / `127.0.0.1:3101` | Profiling toggle and address (loopback only) |

> Unknown variables are ignored rather than fatal, so a `.env` inherited from the upstream Node deployment generally keeps working.

## 5. Data and migrations (interchangeable with upstream)

This fork shares upstream's **PostgreSQL schema and Redis key space**, therefore:

- A database migrated by upstream works as-is: the migration source of truth is still `drizzle/` (including `meta/_journal.json`), and the built-in Go migrator reimplements drizzle's journal parsing, `sha256(sql)` fingerprinting and `--> statement-breakpoint` splitting byte-for-byte. Applied watermarks match drizzle exactly.
- The 18 Lua scripts used for rate limiting and sessions are byte-identical to the Go-side constants, with golden replays comparing terminal state and key shapes.
- **Rolling back** means switching back to the upstream image; no data rollback is needed.

## 6. Measured capacity and performance

Same machine (2 vCPU / 1.9 GB), same mock upstreams, same workload:

| Metric | Upstream (Node) | This fork (Go) |
| --- | --- | --- |
| **Resident delta per stream** (primary figure: independent of the cold-start baseline) | ≈**8.57 MiB/stream** | ≈**0.56 MiB/stream** |
| Image size | 333 MB (332,888,216 B) | **45.8 MB** |
| 50 concurrent × 8 MiB streamed responses, peak RSS (same fixture; **the peak includes the cold-start baseline, so it is not comparable across machines**) | 797.5 MiB (baseline 368.9 + delta 428.6); a second run of the same workload measured 643.6 MiB (baseline 217.5 + delta 426.1) | **52.5 MiB** (baseline 24.7 + delta 27.9) |
| Idle production instance (UI embedded, PG and Redis connected) | ≈300 MiB | **184 MiB** (well within a 768 MiB limit) |

A production instance of this fork (2 vCPU / 1.9 GB) measures 2.5%–4.1% CPU.

> Provenance: the upstream column is a measurement recorded during the rewrite (Node image `cch:0.9.6-local`, Node 22 + Next standalone); the Go column can be reproduced with `docker images` for image size and `scripts/perf-*.sh` plus `scripts/capture-profile.mjs` for memory and profiling. Note that **peaks depend on the cold-start baseline** (the two Node peaks above differ by 154 MiB while their per-stream deltas differ by only 0.05 MiB), so compare machines by **resident delta per stream**. Your concurrency, payload sizes and upstream latency will differ — size memory from your own measurements.

## 7. Development

```
go/                       Go backend (data plane + management plane + migrator + embedded UI)
  cmd/cchd/               process entry point (boot, wiring, draining shutdown)
  internal/               feature packages (guard / route / forward / gate / convert / limit / store …)
src/                      frontend (Next.js App Router, statically exported and embedded by Go)
messages/                 UI strings (zh-CN / en)
drizzle/                  migration source of truth (read and applied by Go at startup)
lua/                      rate-limit and affinity Lua scripts (byte-identical to Go constants)
tests/                    vitest unit tests plus load and conformance fixtures
go/deploy/, dev/          deployment templates and local development compose
scripts/build-ui-*.mjs    UI export and compressed embedding (the two build-chain steps)
```

Common commands:

```bash
# frontend
bun run typecheck && bun run lint && bun run test

# Go (integration tests need the two variables; they skip otherwise)
cd go && gofmt -l . && go vet ./... && go test -timeout 600s ./...

# local PG + Redis
cd dev && make db
```

Go integration tests need a real PostgreSQL and Redis:

```bash
cd go
CCH_TEST_DSN='postgres://user:pass@127.0.0.1:5432/cch_loadtest' \
CCH_TEST_REDIS_URL='redis://127.0.0.1:6379/15' \
go test -timeout 600s ./...
```

## 8. Tracking upstream

```bash
git remote add upstream https://github.com/ding113/claude-code-hub.git
git fetch upstream
# upstream frontend changes usually cherry-pick cleanly; backend changes must be ported to Go
git log --oneline upstream/main -- src/ messages/ | head
```

The frontend (`src/**`, `messages/**`) stays structurally close to upstream so it can be followed; the Go backend is a rewrite and needs semantic ports.

## 9. License and credits

- Released under **MIT**, same as upstream. Upstream copyright belongs to [ding113](https://github.com/ding113) and its contributors.
- The UI and data model come from upstream [claude-code-hub](https://github.com/ding113/claude-code-hub) — thanks to its authors for the original work.
- If you run this in production, consider running the protocol conformance fixtures under `tests/load/` first to confirm they match your provider mix.
