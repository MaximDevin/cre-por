// Command por-run is the local simulation of the CRE Proof-of-Reserve
// workflow.
//
// CRE structure (verified against cre-sdk-go v1.21.0:
// cre.Handler(trigger, callback) + cre.Workflow[C] + cre.InitFn[C]):
//
//	trigger  = CRON schedule (flag -schedule, default "0 */15 * * * *")
//	callback = executeProofOfReserve(config, runtime, payload)
//	capabilities used by the callback = HTTP (JSON-RPC eth_call + REST)
//
// The on-network deployment of this workflow needs a CRE account, which is
// wallet-authenticated; we hold no keys, so this binary runs the identical
// trigger/callback body against the same public endpoints and emits the same
// hash-chained evidence trail. See README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"farmo.dev/cre-por/internal/evidence"
	"farmo.dev/cre-por/internal/por"
)

const (
	agentChecker = "PORChecker"
	agentJudge   = "PORJudge"
	ruleSet      = "por-check/v1"
)

type config struct {
	Schedule    string `json:"schedule"`
	OutDir      string `json:"out_dir"`
	EnforcePass bool   `json:"enforce_pass"`
}

// callback is the workflow body: fetch evidence, run checks, chain the
// reasoning trail, write the report.
func callback(ctx context.Context, cfg config) (*por.Report, *evidence.Trail, error) {
	client := por.NewClient(nil)
	run, err := client.Run(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("workflow callback: %w", err)
	}

	trail, err := evidence.Open(filepath.Join(cfg.OutDir, "trail.jsonl"))
	if err != nil {
		return nil, nil, err
	}

	// persist the evidence bundle (content-addressed records)
	evPath := filepath.Join(cfg.OutDir, "evidence.json")
	var records []map[string]any
	ids := make(map[string]string) // display -> id
	for _, item := range run.Evidence {
		kind, _ := item["kind"].(string)
		source, _ := item["source"].(string)
		url, _ := item["url"].(string)
		payload, _ := item["payload"].(map[string]any)
		ev := evidence.Make(kind, source, url, payload)
		records = append(records, map[string]any{
			"id": ev.ID, "kind": ev.Kind, "source": ev.Source,
			"url": ev.URL, "fetched_at": ev.FetchedAt, "payload": ev.Payload,
		})
		ids[kind+"/"+source] = ev.ID
	}
	// exact JSON-RPC exchanges are evidence too (re-fetchable bytes)
	for _, raw := range run.Raw {
		payload := map[string]any{
			"chain": raw.Chain, "rpc": raw.RPC, "to": raw.To,
			"method": raw.Method, "data": raw.Data, "result": raw.Result,
		}
		ev := evidence.Make("rpc.eth_call", raw.Chain+" JSON-RPC", raw.RPC, payload)
		records = append(records, map[string]any{
			"id": ev.ID, "kind": ev.Kind, "source": ev.Source,
			"url": ev.URL, "fetched_at": ev.FetchedAt, "payload": ev.Payload,
		})
		ids["rpc/"+raw.Chain+"/"+raw.Method] = ev.ID
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, nil, err
	}
	if b, err := json.MarshalIndent(records, "", " "); err == nil {
		_ = os.WriteFile(evPath, b, 0o644)
	}

	// one trail entry per published check, quoting the evidence it consumed
	chainIDs := []string{}
	for k, v := range ids {
		if len(k) >= 6 && k[:6] == "rpc/" {
			chainIDs = append(chainIDs, v)
		}
	}
	for _, chk := range run.Report.Checks {
		_, err := trail.Record(agentChecker, ruleSet, chainIDs,
			[]map[string]any{{"id": chk.ID, "detail": chk.Detail}},
			map[string]any{"check": chk.ID, "pass": chk.Pass, "detail": chk.Detail},
			map[string]any{"schedule": cfg.Schedule})
		if err != nil {
			return nil, nil, err
		}
	}

	verdict := map[string]any{
		"pass":                run.Report.Pass,
		"reserve_btc":         run.Report.ReserveBTC,
		"wrapped_total_btc":   run.Report.WrappedTotalBTC,
		"coverage_ratio":      run.Report.CoverageRatio,
		"price_deviation_pct": run.Report.PriceDeviationPct,
	}
	if _, err := trail.Record(agentJudge, ruleSet, chainIDs,
		[]map[string]any{{"id": "verdict.all_checks", "detail": "every published check must pass"}},
		verdict, map[string]any{"checks_total": len(run.Report.Checks)}); err != nil {
		return nil, nil, err
	}
	run.Report.TrailHead = trail.HeadShort()

	// persist the report
	b, _ := json.MarshalIndent(run.Report, "", " ")
	if err := os.WriteFile(filepath.Join(cfg.OutDir, "report.json"), b, 0o644); err != nil {
		return nil, nil, err
	}
	return &run.Report, trail, nil
}

func main() {
	var cfg config
	flag.StringVar(&cfg.OutDir, "out", "data", "output directory for evidence/report/trail")
	flag.StringVar(&cfg.Schedule, "schedule", "0 */15 * * * *",
		"CRON schedule this workflow is registered with on CRE")
	flag.BoolVar(&cfg.EnforcePass, "enforce", true,
		"exit non-zero when any published check fails")
	flag.Parse()

	start := time.Now()
	report, trail, err := callback(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(2)
	}

	fmt.Println("========================================================================")
	fmt.Println("CRE Proof-of-Reserve workflow - local simulation report")
	fmt.Println("========================================================================")
	fmt.Printf("trigger          : CRON %q (CRE handler: cre.Handler(trigger, callback))\n", cfg.Schedule)
	fmt.Printf("executed at      : %s\n", start.UTC().Format(time.RFC3339))
	fmt.Printf("reserve (BTC)    : %.2f  (CoinGecko circulating supply)\n", report.ReserveBTC)
	for chain, v := range report.WrappedByChain {
		fmt.Printf("wrapped %-9s: %.4f\n", chain, v)
	}
	fmt.Printf("wrapped total    : %.4f BTC\n", report.WrappedTotalBTC)
	fmt.Printf("coverage ratio   : %.4fx  (reserve / wrapped claims)\n", report.CoverageRatio)
	fmt.Printf("price sources    : binance=%.2f coingecko=%.2f dev=%.4f%% (max %.2f%%)\n",
		report.SpotPriceUSD, report.ReservePriceUSD, report.PriceDeviationPct, por.MaxPriceDeviationPct)
	fmt.Println("checks:")
	for _, c := range report.Checks {
		mark := "PASS"
		if !c.Pass {
			mark = "FAIL"
		}
		fmt.Printf("  [%s] %-24s %s\n", mark, c.ID, c.Detail["rule"])
	}
	fmt.Printf("verdict          : %s\n", boolWord(report.Pass))
	if trail != nil {
		if err := trail.Verify(); err != nil {
			fmt.Printf("trail verify     : FAIL (%v)\n", err)
		} else {
			fmt.Printf("trail verify     : ok: %d entries verified, head=%s...\n",
				len(trail.Entries), trail.HeadShort())
		}
	}
	fmt.Printf("outputs          : %s/{evidence.json,report.json,trail.jsonl}\n", cfg.OutDir)

	if !report.Pass && cfg.EnforcePass {
		os.Exit(1)
	}
}

func boolWord(v bool) string {
	if v {
		return "PASS"
	}
	return "FAIL"
}
