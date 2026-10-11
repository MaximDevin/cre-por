# cre-por — Proof-of-Reserve workflow for Chainlink CRE

Submission for **"Best workflow with CRE – 2x $1,000"** (bounty **#1362**,
sponsor Chainlink) in the BLI Legal Tech Hackathon 2 on DoraHacks.

A CRON-triggered workflow that cross-checks **wrapped-BTC (custodial) claims on
two blockchains** against the **BTC circulating reserve** and against **two
independent price sources**, using only free, key-less public endpoints. Every
value it consumes is written as a content-addressed evidence record, and every
check lands in a SHA-256 hash-chained reasoning trail.

- Go 1.27, **standard library only** — `go build ./... && go test ./...` just works
- No API keys, no RPC keys, no private keys, no transactions sent
- Verified live run on 2026-10-09 (output below)

---

## 1. CRE structure

The workflow follows the handler pattern documented in the official CRE
Bootcamp (https://smartcontractkit.github.io/cre-bootcamp-2026/) and verified
against `github.com/smartcontractkit/cre-sdk-go v1.21.0`:

```go
// cre-sdk-go shape (cre.Handler + cre.Workflow[C] + cre.InitFn[C])
return cre.Workflow[Config]{
    cre.Handler(cronTrigger, executeProofOfReserve),
}, nil
```

| CRE concept | This project |
|---|---|
| Trigger | CRON `0 */15 * * * *` (flag `-schedule`; schedule is registration metadata) |
| Callback | `executeProofOfReserve` → `internal/por.Client.Run(ctx)` |
| Capability: HTTP | JSON-RPC `eth_call` (`symbol()`, `decimals()`, `totalSupply()`) + CoinGecko/Binance REST |
| Capability: EVM Read | ERC-20 reads on Ethereum and BNB Chain (no writes, ever) |
| Output | hash-chained verdict + `report.json` |

**The deployable workflow is in `workflow/`** (see §8) and compiles to WASM
against the real SDKs:

```bash
GOOS=wasip1 GOARCH=wasm go build -o por.wasm ./workflow   # -> por.wasm, 15 MB
```

```go
// workflow/workflow.go - the same published rules, wired for the network
func InitWorkflow(config *Config, _ *slog.Logger, _ cre.SecretsProvider) (cre.Workflow[*Config], error) {
	return cre.Workflow[*Config]{
		cre.Handler(cron.Trigger(&cron.Config{Schedule: config.Schedule}), onProofOfReserveCron),
	}, nil
}
```

`cmd/por-run` runs **that exact callback body** locally with the same rules
(`internal/por.Evaluate` is shared, transport-independent), because
registering/running the workflow on the CRE network requires a CRE account that
is **wallet-authenticated** — and we hold no private keys, so we cannot
authenticate (documented blocker, not a shortcut we faked). Everything else is
real: real chain reads, real APIs, real hashing, real WASM build.

## 2. What it checks (published rules, all in `internal/por`)

| id | rule | threshold |
|---|---|---|
| `identity.<chain>` | `symbol()` read on-chain must equal the expected asset (`WBTC`, `BTCB`) | exact match |
| `liveness.<chain>` | wrapped supply must be readable and `> 0` | `> 0` |
| `reserve_floor` | BTC circulating supply ÷ total wrapped BTC | `>= MinCoverageRatio` (1.0) |
| `price_agreement` | Binance vs CoinGecko spot deviation | `<= MaxPriceDeviationPct` (0.5 %) |

Any failing check makes the whole workflow run exit non-zero (`-enforce`, on by
default), which is what a DON would report as a failed report.

## 3. Data sources (free, key-less)

| source | endpoint | used for |
|---|---|---|
| Ethereum public node | `https://ethereum.publicnode.com` | WBTC `symbol/decimals/totalSupply` |
| BNB Chain public seed | `https://bsc-dataseed.binance.org` | BTCB `symbol/decimals/totalSupply` |
| CoinGecko public API | `/api/v3/coins/markets?ids=bitcoin` | reserve: circulating BTC, USD price |
| Binance public REST | `/api/v3/ticker/24hr?symbol=BTCUSDT` | independent USD price |

Contract addresses are **verified at runtime** by reading `symbol()` on-chain,
so a wrong address fails the `identity` check instead of silently poisoning the
report.

## 4. Live run (2026-10-09)

```
========================================================================
CRE Proof-of-Reserve workflow - local simulation report
========================================================================
trigger          : CRON "0 */15 * * * *" (CRE handler: cre.Handler(trigger, callback))
executed at      : 2026-10-09T16:44:26Z
reserve (BTC)    : 20095612.00  (CoinGecko circulating supply)
wrapped bsc      : 65300.9700
wrapped ethereum : 116132.1827
wrapped total    : 181433.1527 BTC
coverage ratio   : 110.7604x  (reserve / wrapped claims)
price sources    : binance=82724.39 coingecko=82683.00 dev=0.0501% (max 0.50%)
checks:
  [PASS] identity.ethereum        symbol() on-chain must match the expected asset
  [PASS] liveness.ethereum        wrapped supply must be readable and greater than zero
  [PASS] identity.bsc             symbol() on-chain must match the expected asset
  [PASS] liveness.bsc             wrapped supply must be readable and greater than zero
  [PASS] reserve_floor            BTC circulating supply / total wrapped BTC >= MinCoverageRatio
  [PASS] price_agreement          Binance vs CoinGecko spot deviation <= MaxPriceDeviationPct
verdict          : PASS
trail verify     : ok: 7 entries verified, head=0af26a3aabfb9c38...
outputs          : data/{evidence.json,report.json,trail.jsonl}
```

```
$ go test ./...
ok  farmo.dev/cre-por/internal/evidence
ok  farmo.dev/cre-por/internal/por
```

## 5. Evidence + hash-chained trail (house style)

Identical to EvidenceTrail (the companion submission for bounty #1380):

- every fetched value becomes an `Evidence` record with
  `id = sha256(canonical_json(payload))` plus its source URL and fetch time;
  the exact JSON-RPC request/response pairs are stored too, so any number can
  be re-fetched and re-hashed;
- every check and the final verdict is appended to `data/trail.jsonl` as
  `hash = sha256(prev_hash || canonical(body))`;
- `Trail.Verify()` re-computes the chain — a unit test proves that editing one
  historical conclusion breaks it (`TestTamperingDetected`).

## 6. Tests

| test | proves |
|---|---|
| `TestChainVerifies` / `TestTamperingDetected` / `TestReloadKeepsChain` | trail chaining, tamper detection, persistence |
| `TestABIDecoding` | ABI string/uint decoding incl. 256-bit supplies (uint64 would overflow at 18 decimals) |
| `TestRunAllChecksPass` | happy path end-to-end over a fake transport |
| `TestRunFailsOnWrongSymbol` | wrong contract → identity check fails |
| `TestRunFailsOnPriceDivergence` | 10 % source divergence fails `price_agreement` |
| `TestRunFailsWhenClaimsExceedReserve` | wrapped > circulating fails `reserve_floor` |

## 7. Run it

```bash
go test ./...                       # 9 tests, offline
go run ./cmd/por-run -out data      # live run against public endpoints
go run ./cmd/por-run -schedule "0 */15 * * * *"   # schedule metadata for registration
```

Outputs: `data/evidence.json`, `data/report.json`, `data/trail.jsonl`.

## 8. On-network deployment (wired, blocked only by account auth)

`workflow/` is a complete CRE workflow project in the official layout
(mirroring `smartcontractkit/cre-templates` Go building blocks):

| file | purpose |
|---|---|
| `workflow/main.go` | `//go:build wasip1` entrypoint: `wasm.NewRunner(cre.ParseJSON[Config]).Run(InitWorkflow)` |
| `workflow/workflow.go` | CRON trigger + callback; every fetch goes through the CRE **HTTP capability** (`crehttp.SendRequest`, consensus-gated), never `net/http` |
| `workflow/config.{staging,production}.json` | schedule + public endpoints + token addresses |
| `workflow/workflow.yaml`, `project.yaml` | CRE CLI target settings (workflow names, RPCs — all public, key-less) |

SDKs used (fetched from proxy.golang.org and compiled against):
`github.com/smartcontractkit/cre-sdk-go v1.0.0`,
`.../capabilities/scheduler/cron v1.0.0-beta.0`,
`.../capabilities/networking/http v1.0.0-beta.0`.

Remaining step (blocked for us): `cre` CLI account creation + workflow
registration, which is wallet-authenticated — we hold no keys by hard rule.
Anyone with a CRE account can register it as-is:

```bash
cre  # login, then register with project.yaml/workflow.yaml targets
```

## 9. Limits & honesty notes

* **On-network deployment is not done here**: registering a workflow with the
  CRE network requires a CRE account, whose authentication is wallet-based. We
  hold no keys (hard rule), so the runnable artifact is the local simulation of
  the identical callback. Wiring instructions: install the CRE CLI
  (docs.chain.link/cre/getting-started/cli-installation), implement the CRON
  trigger with the official trigger SDK, register this callback with
  `cre.Handler`, and reuse `internal/por` unchanged — it is transport-agnostic
  behind `HTTPDoer`.
* This is a **reserve-coverage monitor**, not an attestation: it checks that
  custodial wrapped-BTC claims stay under the circulating reserve floor and
  that two independent price feeds agree. It does not prove custodian holdings
  (that needs the custodian's signed attestations).
* `coverage ratio 110x` is expected: only ~181k BTC is wrapped vs 20.09M
  circulating — the rule is a floor (`circulating/claims >= 1`), i.e. claims can
  never exceed the reserve reference.
* No writes, no transactions, no keys anywhere in this repository.

MIT licensed. Companion submission: EvidenceTrail (bounty #1380).
