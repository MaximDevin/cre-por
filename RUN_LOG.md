# Run log — real terminal output

Captured 2026-10-09 on the submission machine (Go 1.27.1). Live public data, no API keys, no private keys.

## 1. Tests

```
?   	farmo.dev/cre-por/cmd/por-run	[no test files]
ok  	farmo.dev/cre-por/internal/evidence	(cached)
ok  	farmo.dev/cre-por/internal/por	(cached)
```

## 2. Live run: `go run ./cmd/por-run -out data`

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

## 3. Trail head entries (first 2, verbatim)

```
{"prev_hash":"0000000000000000000000000000000000000000000000000000000000000000","body":{"agent":"PORChecker","conclusion":{"check":"identity.ethereum","detail":{"expected":"WBTC","observed":"WBTC","rule":"symbol() on-chain must match the expected asset","token":"0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599"},"pass":true},"evidence_ids":[],"inputs":{"schedule":"0 */15 * * * *"},"rules":[{"detail":{"expected":"WBTC","observed":"WBTC","rule":"symbol() on-chain must match the expected asset","token":"0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599"},"id":"identity.ethereum"}],"ruleset":"por-check/v1","seq":1,"ts":"2026-10-09T16:44:29Z"},"hash":"cd67d19b01ee7deef1a1647e9e871377019b145759fc42b2921159f27fdeda35"}
{"prev_hash":"cd67d19b01ee7deef1a1647e9e871377019b145759fc42b2921159f27fdeda35","body":{"agent":"PORChecker","conclusion":{"check":"liveness.ethereum","detail":{"rule":"wrapped supply must be readable and greater than zero","total_supply":116132.18273262},"pass":true},"evidence_ids":[],"inputs":{"schedule":"0 */15 * * * *"},"rules":[{"detail":{"rule":"wrapped supply must be readable and greater than zero","total_supply":116132.18273262},"id":"liveness.ethereum"}],"ruleset":"por-check/v1","seq":2,"ts":"2026-10-09T16:44:29Z"},"hash":"223d2f470763f1638102476a438cc48875593eff8cdb3304e198be4356bf1efe"}
```

## 4. Report JSON

```
{
 "timestamp": "",
 "reserve_btc": 20095612,
 "wrapped_by_chain": {
  "bsc": 65300.96996478413,
  "ethereum": 116132.18273262
 },
 "wrapped_total_btc": 181433.15269740415,
 "coverage_ratio": 110.76041892694023,
 "reserve_price_usd": 82683,
 "spot_price_usd": 82724.39,
 "price_deviation_pct": 0.05005865776519891,
 "checks": [
  {
   "id": "identity.ethereum",
   "pass": true,
   "detail": {
    "expected": "WBTC",
    "observed": "WBTC",
    "rule": "symbol() on-chain must match the expected asset",
    "token": "0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599"
   }
  },
  {
   "id": "liveness.ethereum",
   "pass": true,
   "detail": {
    "rule": "wrapped supply must be readable and greater than zero",
    "total_supply": 116132.18273262
   }
  },
  {
   "id": "identity.bsc",
   "pass": true,
   "detail": {
    "expected": "BTCB",
    "observed": "BTCB",
    "rule": "symbol() on-chain must match the expected asset",
    "token": "0x7130d2A12B9BCbFAe4f2634d864A1Ee1Ce3Ead9c"
   }
  },
  {
   "id": "liveness.bsc",
   "pass": true,
   "detail": {
    "rule": "wrapped supply must be readable and greater than zero",
    "total_supply": 65300.96996478413
   }
  },
  {
   "id": "reserve_floor",
   "pass": true,
   "detail": {
    "min_coverage": 1,
    "observed_coverage": 110.76041892694023,
    "reserve_btc": 20095612,
    "rule": "BTC circulating supply / total wrapped BTC \u003e= MinCoverageRatio",
    "wrapped_btc": 181433.15269740415
   }
  },
  {
   "id": "price_agreement",
   "pass": true,
   "detail": {
    "binance_usd": 82724.39,
    "coingecko_usd": 82683,
    "max_deviation": 0.5,
    "observed_pct": 0.05005865776519891,
    "rule": "Binance vs CoinGecko spot deviation \u003c= MaxPriceDeviationPct"
   }
  }
 ],
 "pass": true,
 "trail_head": "0af26a3aabfb9c38"
}```

## 5. CRE workflow WASM build (2026-10-11)

```
$ GOOS=wasip1 GOARCH=wasm go build -o por.wasm ./workflow
-rwxr-xr-x 1 znao znao 15008288 Oct 11 11:04 /tmp/por.wasm

$ go build ./... && go vet ./... && go test ./...
?   	farmo.dev/cre-por/cmd/por-run	[no test files]
ok  	farmo.dev/cre-por/internal/evidence	(cached)
ok  	farmo.dev/cre-por/internal/por	(cached)

go.mod requires:
	github.com/smartcontractkit/cre-sdk-go v1.0.0
	github.com/smartcontractkit/cre-sdk-go/capabilities/networking/http v1.0.0-beta.0
	github.com/smartcontractkit/cre-sdk-go/capabilities/scheduler/cron v1.0.0-beta.0
	github.com/smartcontractkit/chainlink-protos/cre/go v0.0.0-20250918131840-564fe2776a35
	google.golang.org/protobuf v1.36.7
```
