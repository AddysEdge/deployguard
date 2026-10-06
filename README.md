# DeployGuard

**DeployGuard runs the same API workload against the current (baseline) and candidate backend releases and detects behavioral, contract, error and performance regressions before deployment.**

It is a single Go CLI meant for a CI release gate. You describe a small, versioned GET/HEAD workload in YAML, point it at two controlled environments, and get a terminal summary, a versioned JSON report, and an exit code that a pipeline can act on.

```text
FAIL         get-user         GET /api/users/42
  behavior     FAIL         candidate differs from baseline: 1 FAIL and 0 WARN finding(s)
               FAIL field_removed        /email string -> missing
  performance  SKIPPED      not configured for this scenario
...
FAIL         search           GET /api/search
  behavior     PASS         candidate matched baseline (status 200, application/json)
  performance  FAIL         regression repeated in all 2 rounds (gate: fail)
               round 1 baseline->candidate  p95 1.4ms -> 142.4ms  delta +141.0ms (+10291.5%)  n=100/100 ok, errors 0/0  [breach]
               round 2 candidate->baseline  p95 11.9ms -> 125.2ms  delta +113.4ms (+956.8%)  n=100/100 ok, errors 0/0  [breach]

Result: FAIL (exit code 1)
```

### What it is not

DeployGuard is deliberately narrow. It is **not** an AI PR reviewer, a Postman clone, a dashboard, a distributed job platform, or a general load tester. It does not capture or replay production traffic, send request bodies, follow redirects, retry failures, or store history.

Traffic replay and response diffing are established ideas, and DeployGuard did not invent them. [Speedscale](https://speedscale.com/) replays captured traffic, [Postman](https://www.postman.com/) runs request collections and assertions, Twitter's [Diffy](https://github.com/opendiffy/diffy) compares a candidate against two instances of the primary to filter noise, and [k6](https://grafana.com/docs/k6/latest/) does load testing. DeployGuard's contribution is a small, inspectable implementation of one core: a baseline-versus-candidate comparison with strict safety limits, a semantic JSON comparator, honest handling of untrustworthy baselines, a two-round latency gate, and a precise CI exit-code contract.

---

## Contents

- [Quick start](#quick-start)
- [CLI usage](#cli-usage)
- [Workload configuration (v1)](#workload-configuration-v1)
- [Outcomes and exit codes](#outcomes-and-exit-codes)
- [How responses are compared](#how-responses-are-compared)
- [Performance methodology](#performance-methodology)
- [JSON report](#json-report)
- [Fixtures and seeded evaluation](#fixtures-and-seeded-evaluation)
- [CI integration](#ci-integration)
- [Security and privacy](#security-and-privacy)
- [Limitations](#limitations)
- [Roadmap](#roadmap)
- [Development](#development)

Architecture, package responsibilities and failure handling are described in [docs/architecture.md](docs/architecture.md).

---

## Quick start

**Prerequisites:** Go 1.27 or newer, plus `bash` and `curl` for the release-gate script. On Ubuntu (including GitHub Actions) these are standard. On Windows, use Git Bash; the script adds `.exe` automatically. No Docker, database, credentials or network access beyond loopback are needed.

**One command: the full demo.** This builds both binaries, starts a baseline fixture and a candidate fixture, waits for readiness, runs DeployGuard, writes a report, and exits with DeployGuard's own exit code:

```bash
bash scripts/release-gate.sh             # clean candidate: PASS, exit 0
bash scripts/release-gate.sh regressed   # regressed candidate: FAIL, exit 1
```

**Step by step** (bash on Linux/macOS or Git Bash on Windows, where the binaries are `bin/deployguard.exe` and `bin/dg-fixture.exe`):

```bash
go build -o bin/deployguard ./cmd/deployguard
go build -o bin/dg-fixture ./cmd/dg-fixture

bin/dg-fixture --variant baseline  --addr 127.0.0.1:8080 &
bin/dg-fixture --variant regressed --addr 127.0.0.1:8081 &

bin/deployguard validate --config examples/deployguard.yaml
bin/deployguard compare --config examples/deployguard.yaml \
  --baseline http://127.0.0.1:8080 --candidate http://127.0.0.1:8081 \
  --report reports/run.json
echo "exit code: $?"            # 1: R1-R5 are detected, R8 is a WARN

kill %1 %2
```

**Seeded evaluation** (starts its own fixtures on free ports):

```bash
go run ./cmd/dg-eval --out reports/eval.json
```

---

## CLI usage

```text
deployguard compare  --config FILE --baseline URL --candidate URL [--report FILE]
                     [--log-level debug|info|warn|error] [--log-format text|json]
deployguard validate --config FILE
deployguard version
deployguard help [compare|validate]
```

- `--baseline` and `--candidate` are **required command-line arguments** and are never read from the workload. The same workload can therefore be pointed at any pair of environments, and a workload file cannot quietly carry a production destination.
- Both URLs must be `http`/`https` origins: a scheme and host with an optional port. A path (other than `/`), query, fragment or `user:password@` is rejected, and errors never echo the URL.
- `validate` decodes and validates the workload and resolves `${ENV}` header references without sending any request.
- The human-readable summary goes to **stdout**. Structured logs (`log/slog`, carrying `run_id`, `phase` and `scenario` attributes) go to **stderr**. The JSON report goes only to the `--report` file, so the streams never mix.

---

## Workload configuration (v1)

One YAML document. Decoding is strict: unknown fields, duplicate keys, wrongly typed values, extra YAML documents and unknown `version` values are all rejected. Every problem is then reported with a field path, for example `scenarios[2] (search).performance.samples: must be between 100 and 5000 (got 50)`.

```yaml
version: 1                       # required; only 1 is supported

settings:                        # optional
  timeout: 5s                    # per-request deadline
  max_response_bytes: 1048576    # bodies larger than this are a body_limit failure
  concurrency: 4                 # functional requests in flight (scenarios run in parallel)
  max_total_requests: 5000       # refuse a workload that plans more requests

scenarios:                       # required, 1-200 entries
  - name: search                 # required, unique, [A-Za-z0-9._-], max 64 chars
    method: GET                  # required: GET or HEAD (POST/PUT/PATCH/DELETE are rejected)
    path: /api/search            # required, root-relative (see rules below)
    query:                       # optional map of string -> string
      q: pen
    headers:                     # optional map; values may use ${ENV_NAME}
      Accept: application/json
      Authorization: "Bearer ${DEPLOYGUARD_TOKEN}"
    timeout: 2s                  # optional per-scenario override
    expect_status: 200           # optional, validated on BOTH targets
    ignore:                      # optional exact RFC 6901 JSON Pointers
      - /requestId
      - /metadata/timestamp
    strict_additions: false      # optional; true makes added fields FAIL instead of WARN
    performance:                 # optional; omit to skip latency measurement
      warmup: 10
      samples: 100
      concurrency: 8
      p95_relative_pct: 50       # required when performance is set
      p95_absolute_ms: 50        # required when performance is set
      gate: fail                 # warn (default) or fail
```

### Defaults and hard limits

| Field | Default | Allowed |
|---|---|---|
| `settings.timeout` | `5s` | > 0, at most `60s` (Go duration string) |
| `settings.max_response_bytes` | `1048576` (1 MiB) | 1 to 16 MiB |
| `settings.concurrency` | `4` | 1 to 16 |
| `settings.max_total_requests` | `5000` | 1 to 100000 |
| scenarios | n/a | 1 to 200 |
| `timeout` (scenario) | `settings.timeout` | > 0, at most `60s` |
| `expect_status` | none | 100 to 599 |
| `performance.warmup` | `10` | 0 to 1000 per target block |
| `performance.samples` | `100` | 100 to 5000 per target per round |
| `performance.concurrency` | `settings.concurrency` | 1 to 16 |
| `performance.p95_relative_pct` | **required** | > 0, at most 1000 |
| `performance.p95_absolute_ms` | **required** | > 0, at most 60000 |
| `performance.gate` | `warn` | `warn` or `fail` |
| config file size | n/a | at most 1 MiB |

Fixed (not configurable) in v1: **2** functional observations per target per scenario; **2** measured performance rounds; up to 100 findings kept per comparison (8 printed per scenario, all in the report); JSON nesting up to 512 levels.

**Planned requests** per scenario are `4 + (performance ? 2 rounds x 2 targets x (warmup + samples) : 0)`. The workload is rejected if the total exceeds `max_total_requests`, so a typo such as `samples: 50000` cannot turn into an accidental load test.

### Paths and URLs

- A `path` must start with `/`, must not start with `//` (that would be a scheme-relative URL naming another host), and must not contain `?` or `#` (use `query`), whitespace, control characters, backslashes, non-ASCII characters, malformed percent-encoding, or `.`/`..` segments (even percent-encoded ones).
- Request URLs are built field by field (`url.URL{Scheme, Host, Path, RawPath, RawQuery}`), never by string concatenation. The result is checked again to confirm it still targets the configured origin.

### Headers and `${ENV}` secrets

- Only header **values** are interpolated, and only in the exact form `${NAME}`, where `NAME` matches `[A-Za-z_][A-Za-z0-9_]*`. Any other `${...}` or an unterminated `${` is an error; a `$` not followed by `{` is literal.
- A referenced variable that is **not set** fails validation before any request is sent. A variable set to an empty string is allowed.
- A resolved value containing CR, LF or other control characters is rejected, and the error never includes the value.
- `Host`, `Content-Length`, `Transfer-Encoding`, `Connection`, `Upgrade`, `TE`, `Trailer`, `Keep-Alive`, `Proxy-Connection`, `Expect`, `Content-Type` and `Content-Encoding` cannot be set. Header names are case-insensitive, so `Accept` and `accept` together are a duplicate.

The fully commented example is [examples/deployguard.yaml](examples/deployguard.yaml). It covers a normal JSON scenario, an explicit ignore of nondeterministic fields, HEAD and non-JSON scenarios, a redirect, and a performance-gated scenario.

---

## Outcomes and exit codes

Each scenario has a **behavior** sub-result, a **performance** sub-result (`SKIPPED` if not configured or not measured), and an overall outcome, which is the worse of the two. `SKIPPED` never counts as evidence of anything.

| Outcome | Meaning |
|---|---|
| `PASS` | Candidate matched the stable, healthy baseline (and latency was within thresholds if measured). |
| `WARN` | A non-blocking difference: an added JSON field (default policy), a latency or error problem in only one of two rounds, or a repeated one with `gate: warn`. |
| `FAIL` | A regression against a trustworthy baseline. |
| `INCONCLUSIVE` | The baseline could not serve as a reference, or the run was canceled, so the release cannot be safely approved. |

Run precedence: **FAIL > INCONCLUSIVE > WARN > PASS**. A FAIL stays a FAIL even when another scenario is inconclusive, and both are listed in the final reasons.

| Exit code | When |
|---|---|
| `0` | PASS or WARN (WARN is intentionally non-blocking) |
| `1` | FAIL |
| `2` | INCONCLUSIVE |
| `3` | Invalid usage or config, report-write failure, or another DeployGuard error |
| `130` | Canceled by SIGINT/SIGTERM (Ctrl+C). In-flight requests are canceled, no new work starts, scenarios that did not finish are INCONCLUSIVE, and the report is still written with `"canceled": true`. A second Ctrl+C terminates immediately. |

The report is written **before** DeployGuard exits with a nonzero release outcome. If it cannot be written, DeployGuard prints a conspicuous `ERROR:` to stderr and exits `3`, even when the verdict was PASS, because a gate missing its evidence should not pass silently.

### Behavioral verdict rules

Each scenario takes four functional observations in the interleaved order **baseline, candidate, baseline, candidate**. The rules, in order:

1. Any observation canceled: **INCONCLUSIVE**.
2. A **baseline** transport failure (timeout, DNS, TLS, refused, reset, network), an oversized body, a **5xx**, a malformed body under a declared JSON media type, or an `expect_status` violation: **INCONCLUSIVE**. There is no trustworthy reference, so even the *same* 5xx on both sides is not a PASS.
3. The two baseline observations differ after ignore rules: **INCONCLUSIVE** (baseline instability, with the differing paths listed). One volatile response is never treated as the contract.
4. The same problems as rule 2 on the **candidate**, against a healthy baseline: **FAIL**.
5. The two candidate observations differ while the baseline was stable: **FAIL** (candidate instability).
6. Otherwise the first baseline and candidate observations are compared. Removed fields, type changes, changed values, array length changes, status, media type and Location changes, and non-JSON body changes are **FAIL**. Added fields are **WARN**, or **FAIL** with `strict_additions: true`.

Performance is measured only when the behavior is PASS or WARN. A behavioral FAIL or INCONCLUSIVE skips it and says why, rather than adding traffic or issuing a misleading latency verdict.

---

## How responses are compared

The JSON comparator is implemented in [internal/compare](internal/compare) without a diff library.

- **Object key order never matters; array order does.** Arrays are compared index by index, and a length change is reported at the array's path.
- **Missing and `null` are different.** `{"manager": null}` to `{}` is `field_removed`; `null` to `"bob"` is `type_changed`.
- **Numbers are exact.** Numbers are parsed from their literal text into a canonical decimal form, never via `float64`. `1`, `1.0`, `1e0` and `-0.0`/`0` are equal; `9007199254740993` and `9007199254740992` are different, and `129.5` versus `"129.5"` is a type change.
- **Strict parsing.** A body with trailing data, duplicate object keys (ambiguous JSON) or nesting deeper than 512 levels counts as malformed.
- **Media types.** The comparator compares the lowercased `type/subtype` and ignores parameters, so `Text/CSV;charset=UTF-8` equals `text/csv; charset=utf-8`. A charset change is therefore *not* detected. `application/json` and any `+json` type are parsed as JSON. Non-JSON bodies are compared byte for byte, and findings show only size and a SHA-256 prefix. HEAD responses have no body to compare.
- **Redirects are never followed.** Status and `Location` are compared. A same-origin Location (relative, or absolute to the target's *own* origin, with default ports normalized) is reduced to its path and query, so `http://baseline/x` and `http://candidate/x` are equal. A cross-origin Location keeps its full origin, and DeployGuard never requests it.
- **Findings use exact RFC 6901 JSON Pointer paths** (`/items/0/qty`, and `/a~1b` for the key `a/b`), sorted deterministically: status, media type and Location first, then body paths.
- **Findings never include scalar values.** They show the category, path and JSON *types* (`string -> missing`, `number -> string`), so a run's output does not copy response data such as emails or tokens into CI logs.

### Ignore rules: exact semantics

`ignore` takes **exact RFC 6901 JSON Pointers**, not JSONPath. Write `/requestId` rather than `$.requestId`; the validator rejects `$`-prefixed paths with that hint. Wildcards are not supported in v1, so an array element is ignored by index (`/items/0/updatedAt`).

An ignore rule suppresses **only** a differing **scalar value** (string, number or boolean) **at exactly that path**, when the path exists on both sides with the **same JSON type**. It never hides:

- a removed or added field at that path,
- a type change (including `null` to a value),
- a whole subtree (an object or array pointer suppresses nothing below it),
- an array length change.

Rules apply both to the baseline-stability check and to the baseline/candidate comparison. The report lists every rule as `applied` (with a count of suppressed differences) or `unused`, with a reason: path not present, path holds an object/array/null, a structural or type change at the path, value never differed, or not evaluated because the baseline was untrustworthy. Configuration therefore cannot silently suppress behavior, or appear to suppress it when it does not.

---

## Performance methodology

Performance is **opt-in per scenario** and runs only after the functional phase, for scenarios whose behavior is PASS or WARN. Scenarios are measured one at a time.

1. **Two rounds with alternating order.** Round 1 measures the baseline and then the candidate; round 2 measures the candidate and then the baseline. Warm-up effects and slow drift therefore do not always favor the same target.
2. **One target at a time.** Each target block runs `warmup` requests (excluded from metrics), then `samples` measured attempts at a fixed `concurrency` (via a bounded worker pool). The baseline and candidate are never loaded simultaneously, which reduces shared-host contention. Both targets get the identical request shape and concurrency.
3. **Duration** runs from immediately before the request is sent through the complete, bounded read of the response body. It includes connection setup when a new connection is needed (each target keeps its own keep-alive pool), server time and transfer. Functional-check durations are recorded separately and never feed the gate.
4. **Valid samples.** A sample counts only if there was no transport failure and the status equals `expect_status`, or, if that is unset, the status the baseline returned during functional checks. **Timeouts are errors**, not slow samples to discard.
5. **Percentiles** use the **nearest-rank** method over successful measured attempts: `p = sorted[ceil(q/100 x n) - 1]`. The report includes p50 and p95 per target per round, plus an informational aggregate. There is no p99 because 100 samples cannot support it.
6. **Per-round classification:**
   - any baseline error (warm-up or measured) in any round: the performance result is **INCONCLUSIVE**;
   - any candidate error in a round: that round is *errored*. Its latency is **not** judged from the successful subset;
   - otherwise the round **breaches** only if candidate p95 exceeds baseline p95 by **more than `p95_absolute_ms` AND more than `p95_relative_pct`**.
7. **Decision.** Both rounds breached or errored: apply `gate` (`warn` by default, `fail` to block). Exactly one round: **WARN** (a suspected, non-repeatable change). Neither: **PASS**.

Both thresholds are required because each alone is misleading on a fast endpoint. In the measured run below, the clean fixture's `/api/search` showed a +1.22 ms p95 difference in round 1, which is about +235% relative, from loopback noise alone. The absolute threshold prevents that from becoming a regression. On a slow endpoint, a large absolute jitter can be a small relative change, which the relative threshold handles.

**What these numbers are, and are not.** The thresholds are *operational* release-gate rules, not statistical tests. DeployGuard computes no confidence intervals or significance and makes no claim of production benchmarking precision. Two rounds of 100 samples reduce the chance that a single noisy burst blocks a release, but they cannot rule out noise. Shared CI runners have noisy neighbors and CPU throttling, and fixtures running on the same machine as the client share its CPU. Tune thresholds to your environment's observed variance, prefer dedicated, repeatable environments for latency gates, and treat a latency PASS as "no large regression observed under these conditions." This mirrors [Grafana's guidance](https://grafana.com/docs/k6/latest/testing-guides/automated-performance-testing/) on repeatable environments and not treating a single pass/fail as complete assurance.

---

## JSON report

`--report FILE` writes indented JSON with `"schema_version": 1`. Parent directories are created, and the file is written to a temporary file and renamed into place, so readers never see a partial report. Field order and finding order are deterministic.

Top level: `schema_version`, `tool` (name, version), `run` (id, started/finished time, duration, `canceled`, config path and version, baseline and candidate origins, planned and issued request counts), `settings` (effective non-secret limits), `outcome`, `exit_code`, `summary` (counts per outcome), `reasons`, and `scenarios[]`.

Each scenario has `name`, `method`, `path`, `query`, `outcome`, plus:

- `behavior`: `outcome`, `reasons`, `expect_status`, `strict_additions`, `observations[]` (sequence, target, status, media type, redacted Location, body size, duration, typed `error`), `findings[]` (`category`, `path`, `severity`, safe `baseline`/`candidate` descriptions), `findings_omitted`, `baseline_instability[]`, `candidate_instability[]`, and `ignore_rules[]` (`path`, `status`, `suppressed`, `note`).
- `performance`: `outcome`, `reasons`, `policy` (rounds, warm-up, samples, concurrency, thresholds, gate, percentile method, valid status), `rounds[]` (order; per target warm-up attempts and errors, attempts, successes, transport and status errors, `error_kinds`, `p50_ms`, `p95_ms`, `wall_ms`; delta in ms and %; `status`: `ok`, `breach`, `candidate_errors`, `baseline_errors` or `incomplete`) and an informational `aggregate`.

The report never contains request headers, resolved environment variables or response bodies.

---

## Fixtures and seeded evaluation

[`cmd/dg-fixture`](cmd/dg-fixture) serves a tiny demo API in three variants: `baseline`, `clean` (harmless changes that must not FAIL: reordered keys, `129.5` versus `129.50`, Content-Type casing, small latency jitter), and `regressed` (one seeded regression per endpoint). Each case has its own endpoint and its own workload in [examples/eval](examples/eval), so each is independently testable.

| ID | Case | Endpoint | Candidate | Expected |
|---|---|---|---|---|
| R1 | removed JSON field | `/api/users/42` | regressed | FAIL |
| R2 | changed JSON type (number to string) | `/api/orders/7` | regressed | FAIL |
| R3 | changed HTTP status (200 to 404) | `/api/items/3` | regressed | FAIL |
| R4 | candidate server error (500) | `/api/inventory` | regressed | FAIL |
| R5 | large injected latency (+120 ms) with `gate: fail` | `/api/search` | regressed | FAIL |
| R6 | changing `requestId`/`timestamp`, ignored at exact paths | `/api/status` | regressed | PASS |
| R7 | reordered JSON object properties | `/api/profile` | regressed | PASS |
| R8 | harmless added field, default policy | `/api/catalog` | regressed | WARN |
| R8s | same added field, `strict_additions: true` | `/api/catalog` | regressed | FAIL |
| I1 | baseline instability (per-request counter) | `/api/unstable` | clean | INCONCLUSIVE |
| I2 | unavailable baseline (refused port) | `/api/users/42` | clean | INCONCLUSIVE |
| N1 | modest noisy latency (0-4 ms jitter), 25 ms gate | `/api/noisy` | clean | PASS |
| E1 | full example workload | all | clean | PASS |
| E2 | full example workload | all | regressed | FAIL |

`go run ./cmd/dg-eval` runs every case through the real engine against real loopback HTTP servers. It compares observed and expected outcomes, prints the measured p95 values, writes `reports/eval.json`, and exits nonzero on any mismatch. CI runs it, and `internal/eval` runs it as a Go test.

### Measured results

These values come from one actual run, not hand-written numbers. Re-run the command to reproduce on your machine; latency values will differ.

- **Date:** 2026-10-06 · **Machine:** Windows 11 Home, AMD Ryzen AI 7 350 (16 logical CPUs) · **Go:** 1.27.0 windows/amd64 · fixtures on loopback in the same process.
- **R5 and E2 policy:** warm-up 10, 100 samples per target per round, concurrency 8, breach if > 50 ms **and** > 50%, `gate: fail`. **N1 policy:** warm-up 10, 100 samples, concurrency 4, > 25 ms **and** > 50%, `gate: fail`.

**Outcome accuracy on the seeded suite: 14/14 cases matched.**

| Kind | Matched | Regressions detected | False negatives | False positives |
|---|---|---|---|---|
| behavior | 10/10 | 5/5 | 0 | 0 of 3 non-regression cases |
| performance | 2/2 | 1/1 | 0 | 0 of 1 non-regression case |
| workload (full example) | 2/2 | 1/1 | 0 | 0 of 1 non-regression case |

I1 and I2 are labelled *untrustworthy*: the correct answer is INCONCLUSIVE rather than a detection decision, and both matched.

**Measured p95** (nearest-rank; n = 100 successful attempts per target per round in every row):

| Case | Round | Baseline p95 | Candidate p95 | Delta | Round status |
|---|---|---|---|---|---|
| R5 | 1 (B to C) | 1.66 ms | 121.80 ms | +120.14 ms | breach |
| R5 | 2 (C to B) | 1.78 ms | 121.44 ms | +119.66 ms | breach |
| N1 | 1 | 0.62 ms | 4.12 ms | +3.50 ms | ok |
| N1 | 2 | 0.55 ms | 4.20 ms | +3.66 ms | ok |
| E1 `search` | 1 | 0.52 ms | 1.74 ms | +1.22 ms | ok |
| E1 `search` | 2 | 0.64 ms | 1.02 ms | +0.38 ms | ok |
| E2 `search` | 1 | 1.55 ms | 122.83 ms | +121.28 ms | breach |
| E2 `search` | 2 | 3.72 ms | 124.09 ms | +120.38 ms | breach |

This is a check of a **small seeded suite with known answers**, built to show the implementation behaves as specified. It is **not** a measure of general detection accuracy on real APIs. The latency cases use loopback fixtures with an artificial delay far above the threshold. Behavioral and performance results are kept separate.

---

## CI integration

Two GitHub Actions workflows are included. Both pass [actionlint](https://github.com/rhysd/actionlint). Both have also passed on GitHub-hosted runners: the first push to `main` on 2026-10-06 ran CI (Ubuntu including `-race`, plus Windows) and the clean-candidate release gate. The `regressed` manual run is the way to see the gate fail.

**[`.github/workflows/ci.yml`](.github/workflows/ci.yml)** runs on push and pull request:

- On Ubuntu: `gofmt` check, `go vet`, `go build`, `go test ./...`, `go test -race ./...`, then the seeded evaluation, with `reports/eval.json` uploaded as an artifact.
- On Windows: `go test ./...`.
- Detecting the intentionally regressed fixture is an *asserted expected result* in these tests, so CI stays green when DeployGuard works correctly.

**[`.github/workflows/release-gate.yml`](.github/workflows/release-gate.yml)** is a genuine release gate. It calls the CI-agnostic [`scripts/release-gate.sh`](scripts/release-gate.sh), which:

1. builds `deployguard` and `dg-fixture`;
2. starts the baseline and candidate fixtures;
3. waits until each `/health` reports the expected variant, so a stale process on the port cannot impersonate it;
4. runs `deployguard compare ... --report reports/release-gate.json`;
5. exits with DeployGuard's exit code, cleaning up the fixtures through a trap.

The workflow uploads the report with `if: always()`, so the evidence is available even when the gate fails. It does **not** use `continue-on-error`: a FAIL (1) or INCONCLUSIVE (2) fails the job.

- **On push to `main`** it gates the **clean** candidate, which should pass.
- **To watch it block a release,** open *Actions -> Release gate (example) -> Run workflow* and choose `regressed`. Locally the equivalent is `bash scripts/release-gate.sh regressed` (exit 1).

To gate a real service in any CI, run `deployguard compare` against your deployed baseline and candidate *staging* origins, keep the report as an artifact, and let the exit code decide.

---

## Security and privacy

- **Target controlled environments only** (staging, previews, fixtures). GET and HEAD are the only methods, but a poorly designed API can still change state on GET. Never aim DeployGuard at production, and never at a system you are not authorized to test.
- No request bodies, no captured traffic, no automatic redirects (cross-origin Locations are never requested) and **no automatic retries**, which could mask regressions.
- **TLS verification always uses the system trust store.** There is no insecure-skip-verify option in v1. Standard `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` environment variables are honored by Go's HTTP transport.
- **Secrets:** credentials belong in environment variables referenced as `${NAME}` in header values. URLs with `user:password@` are rejected. Resolved values are kept in an unexported field whose `String`/`GoString` methods redact it, and they never appear in errors, logs, terminal output or the report. The E2E tests check stdout, stderr (at debug log level) and the report for a planted secret.
- **No raw headers or bodies are logged or reported.** Findings carry JSON types, sizes and digests, not values. Redirect Locations are shown with query values redacted. Transport errors omit the request URL.
- Sensitive header names (`Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, and names containing `api-key`, `token`, `secret`, `session`, `password` or `credential`) are flagged as sensitive in addition to any `${ENV}`-interpolated header.
- Bounded everything: per-request deadlines, response bytes, in-flight requests, samples and total planned requests.

---

## Limitations

- GET and HEAD only; no request bodies, request chaining or sessions.
- Ignore rules are exact pointers; there are no wildcards, so each array element is listed by index. Arrays are always order-sensitive.
- Media type parameters (including charset) are not compared. Response headers other than Content-Type and Location are not compared.
- Findings deliberately omit values. To see *what* a value changed to, reproduce the request yourself.
- Two functional observations per target catch volatile fields and flapping responses but not rare intermittent failures.
- The latency gate is a two-round operational threshold on p95 from a single client machine. It is not a statistical test or a load test.
- JSON with duplicate keys, or numbers with exponents beyond ±10^9, is treated as malformed.

## Roadmap

- Opt-in, redacted value display for chosen paths when debugging a FAIL.
- Order-insensitive array comparison by a configured key (for example `id`), with the same structural guarantees as ignore rules.
- JUnit XML output alongside the JSON report for CI test-result UIs.
- Optional explicit request bodies for idempotent methods, behind a separate safety review.

---

## Development

```bash
gofmt -l .               # should print nothing
go vet ./...
go test ./...            # includes compiled-CLI end-to-end tests and the seeded suite
go test -short ./...     # skips the seeded suite (~10 s)
go test -race ./...      # requires cgo and a C compiler (runs in CI on Ubuntu)
go run ./cmd/dg-eval
```

Project layout:

```text
cmd/deployguard     CLI (compare, validate, version, help)
cmd/dg-fixture      demo fixture server
cmd/dg-eval         seeded evaluation harness
internal/config     strict YAML decoding, validation, limits, ${ENV} headers, origin checks
internal/replay     safe HTTP execution, failure classification, bounded worker pool
internal/compare    JSON parser, semantic diff, JSON Pointer, media type and Location handling
internal/perf       measured rounds, nearest-rank percentiles, two-round gate
internal/engine     orchestration and behavioral verdict rules
internal/verdict    outcomes, precedence, exit codes
internal/report     JSON report schema, atomic writer, terminal renderer
internal/fixture    fixture handlers (R1-R8, I1, N1)
internal/eval       seeded cases with ground truth
examples/           example workload and one workload per seeded case
scripts/            CI-agnostic release-gate script
```
