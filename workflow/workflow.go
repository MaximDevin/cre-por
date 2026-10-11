//go:build wasip1

package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	crehttp "github.com/smartcontractkit/cre-sdk-go/capabilities/networking/http"
	"github.com/smartcontractkit/cre-sdk-go/capabilities/scheduler/cron"
	"github.com/smartcontractkit/cre-sdk-go/cre"

	"farmo.dev/cre-por/internal/por"
)

// Config is the workflow configuration delivered by CRE
// (cre.ParseJSON[Config] on the runner, workflow/config.*.json in the CLI).
type Config struct {
	Schedule     string `json:"schedule"`
	CoinGeckoURL string `json:"coinGeckoUrl"`
	BinanceURL   string `json:"binanceUrl"`

	EthereumRPC  string `json:"ethereumRpc"`
	EthereumWBTC string `json:"ethereumWbtc"`

	BscRPC  string `json:"bscRpc"`
	BscBTCB string `json:"bscBtcb"`

	ExpectWBTC string `json:"expectWbtc"`
	ExpectBTCB string `json:"expectBtcb"`
}

// DefaultConfig matches the free, key-less endpoints used by cmd/por-run.
func DefaultConfig() *Config {
	return &Config{
		Schedule:     "0 */15 * * * *",
		CoinGeckoURL: por.CoinGeckoURL,
		BinanceURL:   por.BinanceURL,
		EthereumRPC:  por.DefaultChains[0].RPC,
		EthereumWBTC: por.DefaultChains[0].Token,
		ExpectWBTC:   por.DefaultChains[0].ExpectSymbol,
		BscRPC:       por.DefaultChains[1].RPC,
		BscBTCB:      por.DefaultChains[1].Token,
		ExpectBTCB:   por.DefaultChains[1].ExpectSymbol,
	}
}

// InitWorkflow wires the CRON trigger to the callback: cre.Handler(trigger,
// callback) - the documented CRE structure.
func InitWorkflow(config *Config, _ *slog.Logger, _ cre.SecretsProvider) (cre.Workflow[*Config], error) {
	if config.Schedule == "" {
		config = DefaultConfig()
		config.Schedule = "0 */15 * * * *"
	}
	return cre.Workflow[*Config]{
		cre.Handler(
			cron.Trigger(&cron.Config{Schedule: config.Schedule}),
			onProofOfReserveCron,
		),
	}, nil
}

// getReq / postReq are the per-request configs passed through the capability.
type getReq struct{ URL string }
type postReq struct {
	URL  string
	Body string
}

func doGET(cfg getReq, _ *slog.Logger, sr *crehttp.SendRequester) (string, error) {
	resp, err := sr.SendRequest(&crehttp.Request{
		Url: cfg.URL, Method: "GET",
		Headers: map[string]string{"Accept": "application/json"},
	}).Await()
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", cfg.URL, err)
	}
	if resp.GetStatusCode() != 200 {
		return "", fmt.Errorf("GET %s -> %d", cfg.URL, resp.GetStatusCode())
	}
	return string(resp.GetBody()), nil
}

func doPOST(cfg postReq, _ *slog.Logger, sr *crehttp.SendRequester) (string, error) {
	resp, err := sr.SendRequest(&crehttp.Request{
		Url: cfg.URL, Method: "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    []byte(cfg.Body),
	}).Await()
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", cfg.URL, err)
	}
	if resp.GetStatusCode() != 200 {
		return "", fmt.Errorf("POST %s -> %d", cfg.URL, resp.GetStatusCode())
	}
	return string(resp.GetBody()), nil
}

// ethCall builds the JSON-RPC body for one ERC-20 view call.
func ethCall(to, data string) string {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_call",
		"params": []any{map[string]string{"to": to, "data": data}, "latest"},
	})
	return string(b)
}

// rpcResult extracts the JSON-RPC "result" hex string.
func rpcResult(body string) (string, error) {
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return "", fmt.Errorf("rpc json: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("rpc error %d: %s", out.Error.Code, out.Error.Message)
	}
	return out.Result, nil
}

// readTokenViaCapability performs the three ERC-20 reads through the CRE HTTP
// capability (one consensus-gated call per request).
func readTokenViaCapability(config *Config, runtime cre.Runtime, client *crehttp.Client,
	chainName, rpcURL, token, expect string) (por.TokenRead, error) {

	call := func(data string) (string, error) {
		body, err := crehttp.SendRequest(postReq{URL: rpcURL, Body: ethCall(token, data)},
			runtime, client, doPOST, cre.ConsensusIdenticalAggregation[string]()).Await()
		if err != nil {
			return "", err
		}
		return rpcResult(body)
	}
	symbolHex, err := call("0x95d89b41") // symbol()
	if err != nil {
		return por.TokenRead{}, fmt.Errorf("%s symbol: %w", chainName, err)
	}
	decimalsHex, err := call("0x313ce567") // decimals()
	if err != nil {
		return por.TokenRead{}, fmt.Errorf("%s decimals: %w", chainName, err)
	}
	supplyHex, err := call("0x18160ddd") // totalSupply()
	if err != nil {
		return por.TokenRead{}, fmt.Errorf("%s totalSupply: %w", chainName, err)
	}
	read, err := por.ParseTokenRead(por.Chain{Name: chainName, RPC: rpcURL, Token: token},
		symbolHex, decimalsHex, supplyHex)
	if err != nil {
		return por.TokenRead{}, err
	}
	read.ExpectedSymbol = expect
	return read, nil
}

// onProofOfReserveCron is the workflow callback: fetch (capability) ->
// evaluate (shared published rules) -> report.
func onProofOfReserveCron(config *Config, runtime cre.Runtime, _ *cron.Payload) (string, error) {
	logger := runtime.Logger()
	client := &crehttp.Client{}
	started := time.Now().UTC().Format(time.RFC3339)

	cgBody, err := crehttp.SendRequest(getReq{URL: config.CoinGeckoURL}, runtime, client,
		doGET, cre.ConsensusIdenticalAggregation[string]()).Await()
	if err != nil {
		return "", fmt.Errorf("coinGecko: %w", err)
	}
	var mktRows []struct {
		CirculatingSupply float64 `json:"circulating_supply"`
		CurrentPrice      float64 `json:"current_price"`
	}
	if err := json.Unmarshal([]byte(cgBody), &mktRows); err != nil || len(mktRows) == 0 {
		return "", fmt.Errorf("coinGecko parse: rows=%d err=%v", len(mktRows), err)
	}
	mkt := por.MarketSnapshot{CirculatingSupply: mktRows[0].CirculatingSupply,
		CurrentPrice: mktRows[0].CurrentPrice}

	bnBody, err := crehttp.SendRequest(getReq{URL: config.BinanceURL}, runtime, client,
		doGET, cre.ConsensusIdenticalAggregation[string]()).Await()
	if err != nil {
		return "", fmt.Errorf("binance: %w", err)
	}
	var tick por.BinanceTicker
	if err := json.Unmarshal([]byte(bnBody), &tick); err != nil {
		return "", fmt.Errorf("binance parse: %w", err)
	}

	ethRead, err := readTokenViaCapability(config, runtime, client,
		"ethereum", config.EthereumRPC, config.EthereumWBTC, config.ExpectWBTC)
	if err != nil {
		return "", err
	}
	bscRead, err := readTokenViaCapability(config, runtime, client,
		"bsc", config.BscRPC, config.BscBTCB, config.ExpectBTCB)
	if err != nil {
		return "", err
	}

	report := por.Evaluate(mkt, tick, []por.TokenRead{ethRead, bscRead})
	logger.Info("proof-of-reserve evaluated",
		"started", started, "pass", report.Pass,
		"coverage", report.CoverageRatio, "deviation_pct", report.PriceDeviationPct)

	out, err := json.Marshal(map[string]any{
		"started_at": started, "schedule": config.Schedule, "report": report,
	})
	if err != nil {
		return "", err
	}
	if !report.Pass {
		// surface the failing checks in the error so the DON reports a fault
		failed := []string{}
		for _, c := range report.Checks {
			if !c.Pass {
				failed = append(failed, c.ID)
			}
		}
		return string(out), fmt.Errorf("proof-of-reserve checks failed: %v", failed)
	}
	return string(out), nil
}
