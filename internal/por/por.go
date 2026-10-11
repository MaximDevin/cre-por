// Package por implements the Proof-of-Reserve checks for the CRE workflow.
//
// Data sources are free and key-less, so the workflow is reproducible by
// anyone:
//
//   - Ethereum JSON-RPC (public node)   -> WBTC totalSupply()/symbol()/decimals()
//   - BNB Chain JSON-RPC (public seed)  -> BTCB totalSupply()/symbol()/decimals()
//   - CoinGecko public API             -> BTC circulating supply (reserve floor), USD price
//   - Binance public API               -> BTCUSDT 24h price (independent cross-check)
//
// The checks and their thresholds are published below - no hidden parameters.
package por

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// Thresholds (published, editable).
const (
	// MaxPriceDeviationPct: Binance vs CoinGecko spot must agree within this.
	MaxPriceDeviationPct = 0.5
	// MinCoverageRatio: wrapped (custodial) BTC claims must stay at or below
	// this fraction of BTC circulating supply (reserve floor check).
	MinCoverageRatio = 1.0
	// Timeout for every outbound call.
	HTTPTimeout = 20 * time.Second
)

// Chain describes one blockchain whose wrapped-BTC supply we read on-chain.
type Chain struct {
	Name string `json:"chain"`
	RPC  string `json:"rpc"`
	// AltRPC lists fallback public endpoints, tried in order when the primary
	// fails (public RPCs are occasionally down or TLS-throttled).
	AltRPC []string `json:"alt_rpc,omitempty"`
	// Token is the wrapped-BTC contract address on that chain.
	Token string `json:"token"`
	// ExpectSymbol is what symbol() must return - an address/identity check.
	ExpectSymbol string `json:"expect_symbol"`
}

// DefaultChains are the wrapped-BTC claims we monitor.
// Addresses are verified at runtime by reading symbol() on-chain.
var DefaultChains = []Chain{
	{Name: "ethereum", RPC: "https://ethereum.publicnode.com",
		AltRPC: []string{"https://eth.llamarpc.com"},
		Token:  "0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599", ExpectSymbol: "WBTC"},
	{Name: "bsc", RPC: "https://bsc-dataseed.binance.org",
		AltRPC: []string{"https://bsc-rpc.publicnode.com"},
		Token:  "0x7130d2A12B9BCbFAe4f2634d864A1Ee1Ce3Ead9c", ExpectSymbol: "BTCB"},
}

// CoinGeckoURL returns the public market snapshot for BTC.
const CoinGeckoURL = "https://api.coingecko.com/api/v3/coins/markets?vs_currency=usd&ids=bitcoin"

// BinanceURL returns the public 24h ticker for BTCUSDT.
const BinanceURL = "https://api.binance.com/api/v3/ticker/24hr?symbol=BTCUSDT"

// Selector constants for the ERC-20 calls we make.
const (
	selSymbol      = "0x95d89b41"
	selTotalSupply = "0x18160ddd"
	selDecimals    = "0x313ce567"
)

// HTTPDoer is the narrow HTTP interface (mockable in tests).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client carries the transport and the endpoints.
type Client struct {
	HTTP      HTTPDoer
	Chains    []Chain
	CoinGecko string
	Binance   string
}

// NewClient builds a client with default chains and public endpoints.
func NewClient(h HTTPDoer) *Client {
	if h == nil {
		h = &http.Client{Timeout: HTTPTimeout}
	}
	chains := make([]Chain, len(DefaultChains))
	copy(chains, DefaultChains)
	return &Client{HTTP: h, Chains: chains, CoinGecko: CoinGeckoURL, Binance: BinanceURL}
}

// RawCall records the exact JSON-RPC exchange for the evidence trail.
type RawCall struct {
	Chain  string `json:"chain"`
	RPC    string `json:"rpc"`
	To     string `json:"to"`
	Method string `json:"method"`
	Data   string `json:"data"`
	Result string `json:"result"`
}

func (c *Client) ethCallOnce(ctx context.Context, rpcURL string, chain Chain, data string) (string, error) {
	payload := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_call",
		"params": []any{map[string]string{"to": chain.Token, "data": data}, "latest"},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cre-por/1.0 (proof-of-reserve workflow)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s rpc %s: %w", chain.Name, rpcURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("%s rpc: bad json: %w", chain.Name, err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("%s rpc: %s (%d)", chain.Name, out.Error.Message, out.Error.Code)
	}
	return out.Result, nil
}

// ethCall tries the primary endpoint and then each fallback, one retry each,
// and reports which endpoint served the answer (recorded in the evidence).
func (c *Client) ethCall(ctx context.Context, chain Chain, method, data string) (string, string, error) {
	endpoints := append([]string{chain.RPC}, chain.AltRPC...)
	var errs []string
	for _, rpcURL := range endpoints {
		for attempt := range 2 {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return "", "", ctx.Err()
				case <-time.After(2 * time.Second):
				}
			}
			res, err := c.ethCallOnce(ctx, rpcURL, chain, data)
			if err == nil {
				return res, rpcURL, nil
			}
			errs = append(errs, err.Error())
		}
	}
	return "", "", fmt.Errorf("all endpoints failed: %s", strings.Join(errs, " | "))
}

// decodeUintABI parses a 32-byte word result as an arbitrary-precision
// unsigned integer (totalSupply can exceed uint64 once scaled by 18 decimals).
func decodeUintABI(hexResult string) (*big.Int, error) {
	if len(hexResult) < 66 || !strings.HasPrefix(hexResult, "0x") {
		return nil, fmt.Errorf("unexpected eth_call result %q", hexResult)
	}
	v := new(big.Int)
	if _, ok := v.SetString(hexResult[2:], 16); !ok {
		return nil, fmt.Errorf("cannot parse %q", hexResult)
	}
	return v, nil
}

// bigToFloat converts a raw on-chain integer to a human unit via decimals,
// using rational arithmetic so no precision is lost before the final divide.
func bigToFloat(v *big.Int, decimals int) float64 {
	if v == nil {
		return 0
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	rat := new(big.Rat).SetFrac(v, den)
	f, _ := rat.Float64()
	return f
}

// decodeStringABI parses an ABI-encoded string (symbol()).
func decodeStringABI(hexResult string) string {
	if len(hexResult) < 130 || hexResult[:2] != "0x" {
		return ""
	}
	raw := hexResult[2:]
	// skip offset word, read length word
	var offset, length uint64
	fmt.Sscanf(raw[0:64], "%64x", &offset)
	fmt.Sscanf(raw[64:128], "%64x", &length)
	if length == 0 || length > 64 || 128+int(length)*2 > len(raw) {
		return ""
	}
	out := make([]byte, length)
	fmt.Sscanf(raw[128:128+int(length)*2], "%X", &out)
	return string(out)
}

// ParseTokenRead assembles a TokenRead from raw eth_call results. It is the
// seam between a transport (net/http locally, the CRE HTTP capability on
// network) and the published checks.
func ParseTokenRead(chain Chain, symbolHex, decimalsHex, supplyHex string) (TokenRead, error) {
	read := TokenRead{RawCall: RawCall{Chain: chain.Name, RPC: chain.RPC, To: chain.Token}}
	read.Symbol = decodeStringABI(symbolHex)
	d, err := decodeUintABI(decimalsHex)
	if err != nil {
		return read, fmt.Errorf("%s decimals: %w", chain.Name, err)
	}
	read.Decimals = int(d.Int64())
	raw, err := decodeUintABI(supplyHex)
	if err != nil {
		return read, fmt.Errorf("%s totalSupply: %w", chain.Name, err)
	}
	read.SupplyRaw = supplyHex
	read.Supply = bigToFloat(raw, read.Decimals)
	return read, nil
}

// TokenRead is one verified on-chain reading.
type TokenRead struct {
	RawCall
	Chain          string  `json:"chain"`
	Token          string  `json:"token"`
	ExpectedSymbol string  `json:"expected_symbol,omitempty"`
	Symbol         string  `json:"symbol"`
	Decimals       int     `json:"decimals"`
	SupplyRaw      string  `json:"supply_raw"`
	Supply         float64 `json:"supply"`
}

// ReadToken fetches symbol(), decimals() and totalSupply() for one chain.
func (c *Client) ReadToken(ctx context.Context, chain Chain) (TokenRead, error) {
	read := TokenRead{RawCall: RawCall{Chain: chain.Name, RPC: chain.RPC, To: chain.Token}}
	var err error
	var used string
	if read.Result, used, err = c.ethCall(ctx, chain, "symbol", selSymbol); err != nil {
		return read, err
	}
	read.RPC = used
	read.Method = "symbol()"
	read.Data = selSymbol
	read.Symbol = decodeStringABI(read.Result)
	if read.Result, _, err = c.ethCall(ctx, chain, "decimals", selDecimals); err != nil {
		return read, err
	}
	d, err := decodeUintABI(read.Result)
	if err != nil {
		return read, fmt.Errorf("%s decimals: %w", chain.Name, err)
	}
	read.Method = "decimals()"
	read.Data = selDecimals
	read.Decimals = int(d.Int64())
	if read.Result, _, err = c.ethCall(ctx, chain, "totalSupply", selTotalSupply); err != nil {
		return read, err
	}
	read.Method = "totalSupply()"
	read.Data = selTotalSupply
	raw, err := decodeUintABI(read.Result)
	if err != nil {
		return read, fmt.Errorf("%s totalSupply: %w", chain.Name, err)
	}
	read.SupplyRaw = read.Result
	read.Supply = bigToFloat(raw, read.Decimals)
	return read, nil
}

// MarketSnapshot is the CoinGecko side of the check.
type MarketSnapshot struct {
	CirculatingSupply float64 `json:"circulating_supply"`
	CurrentPrice      float64 `json:"current_price"`
}

// ReadCoinGecko fetches BTC market data.
func (c *Client) ReadCoinGecko(ctx context.Context) (MarketSnapshot, error) {
	var out []struct {
		CirculatingSupply float64 `json:"circulating_supply"`
		CurrentPrice      float64 `json:"current_price"`
	}
	if err := c.getJSON(ctx, c.CoinGecko, &out); err != nil {
		return MarketSnapshot{}, err
	}
	if len(out) == 0 {
		return MarketSnapshot{}, fmt.Errorf("coingecko returned no rows")
	}
	return MarketSnapshot{CirculatingSupply: out[0].CirculatingSupply,
		CurrentPrice: out[0].CurrentPrice}, nil
}

// BinanceTicker is the Binance side of the price cross-check. Binance returns
// numeric fields as JSON strings, so decode manually.
type BinanceTicker struct {
	LastPrice float64 `json:"lastPrice"`
}

func (t *BinanceTicker) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v, ok := raw["lastPrice"]
	if !ok {
		return fmt.Errorf("binance ticker: lastPrice missing")
	}
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		_, err := fmt.Sscanf(s, "%f", &t.LastPrice)
		return err
	}
	return json.Unmarshal(v, &t.LastPrice)
}

// ReadBinance fetches BTCUSDT last price.
func (c *Client) ReadBinance(ctx context.Context) (BinanceTicker, error) {
	var t BinanceTicker
	if err := c.getJSON(ctx, c.Binance, &t); err != nil {
		return BinanceTicker{}, err
	}
	return t, nil
}

func (c *Client) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "cre-por/1.0 (proof-of-reserve workflow)")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s -> %d", url, resp.StatusCode)
	}
	return json.Unmarshal(raw, v)
}

// CheckResult is one published rule and its outcome.
type CheckResult struct {
	ID     string         `json:"id"`
	Pass   bool           `json:"pass"`
	Detail map[string]any `json:"detail"`
}

// Report is the final, trail-linked verdict of one workflow run.
type Report struct {
	Timestamp         string             `json:"timestamp"`
	ReserveBTC        float64            `json:"reserve_btc"`
	WrappedByChain    map[string]float64 `json:"wrapped_by_chain"`
	WrappedTotalBTC   float64            `json:"wrapped_total_btc"`
	CoverageRatio     float64            `json:"coverage_ratio"`
	ReservePriceUSD   float64            `json:"reserve_price_usd"`
	SpotPriceUSD      float64            `json:"spot_price_usd"`
	PriceDeviationPct float64            `json:"price_deviation_pct"`
	Checks            []CheckResult      `json:"checks"`
	Pass              bool               `json:"pass"`
	TrailHead         string             `json:"trail_head"`
}

// RunResult bundles the verdict with the evidence items consumed by it.
type RunResult struct {
	Report   Report           `json:"report"`
	Evidence []map[string]any `json:"evidence"` // serialized evidence records
	Raw      []RawCall        `json:"raw"`      // exact RPC exchanges
}

// Evaluate applies the published checks to already-fetched values. It is the
// transport-independent core shared by the local runner and the CRE workflow.
func Evaluate(mkt MarketSnapshot, tick BinanceTicker, reads []TokenRead) Report {
	rep := Report{WrappedByChain: map[string]float64{}}
	var checks []CheckResult
	total := 0.0
	for _, read := range reads {
		rep.WrappedByChain[read.Chain] = read.Supply
		total += read.Supply
		expect := read.ExpectedSymbol
		checks = append(checks, CheckResult{
			ID: "identity." + read.Chain, Pass: expect == "" || read.Symbol == expect,
			Detail: map[string]any{"expected": expect, "observed": read.Symbol,
				"token": read.Token, "rule": "symbol() on-chain must match the expected asset"},
		})
		checks = append(checks, CheckResult{
			ID: "liveness." + read.Chain, Pass: read.Supply > 0,
			Detail: map[string]any{"total_supply": read.Supply,
				"rule": "wrapped supply must be readable and greater than zero"},
		})
	}
	rep.ReserveBTC = mkt.CirculatingSupply
	rep.ReservePriceUSD = mkt.CurrentPrice
	rep.WrappedTotalBTC = total
	if total > 0 {
		rep.CoverageRatio = mkt.CirculatingSupply / total
	}
	checks = append(checks, CheckResult{
		ID: "reserve_floor", Pass: rep.CoverageRatio >= MinCoverageRatio,
		Detail: map[string]any{
			"rule":              "BTC circulating supply / total wrapped BTC >= MinCoverageRatio",
			"min_coverage":      MinCoverageRatio,
			"observed_coverage": rep.CoverageRatio,
			"reserve_btc":       mkt.CirculatingSupply, "wrapped_btc": total},
	})
	rep.SpotPriceUSD = tick.LastPrice
	dev := 0.0
	if mkt.CurrentPrice > 0 {
		dev = 100 * abs(tick.LastPrice-mkt.CurrentPrice) / mkt.CurrentPrice
	}
	rep.PriceDeviationPct = dev
	checks = append(checks, CheckResult{
		ID: "price_agreement", Pass: dev <= MaxPriceDeviationPct,
		Detail: map[string]any{
			"rule":          "Binance vs CoinGecko spot deviation <= MaxPriceDeviationPct",
			"max_deviation": MaxPriceDeviationPct,
			"observed_pct":  dev,
			"binance_usd":   tick.LastPrice, "coingecko_usd": mkt.CurrentPrice},
	})
	rep.Checks = checks
	rep.Pass = true
	for _, c := range checks {
		if !c.Pass {
			rep.Pass = false
		}
	}
	return rep
}

// Run performs the full check with the net/http transport and returns the
// report plus the evidence it consumed.
func (c *Client) Run(ctx context.Context) (RunResult, error) {
	res := RunResult{}
	add := func(kind, source, url string, payload map[string]any) {
		res.Evidence = append(res.Evidence, map[string]any{
			"kind": kind, "source": source, "url": url, "payload": payload})
	}

	mkt, err := c.ReadCoinGecko(ctx)
	if err != nil {
		return res, fmt.Errorf("reserve read: %w", err)
	}
	add("reserve.coingecko", "CoinGecko public API", c.CoinGecko, map[string]any{
		"circulating_supply": mkt.CirculatingSupply, "usd": mkt.CurrentPrice})

	var reads []TokenRead
	for _, chain := range c.Chains {
		read, err := c.ReadToken(ctx, chain)
		if err != nil {
			return res, fmt.Errorf("chain %s: %w", chain.Name, err)
		}
		read.Chain = chain.Name
		read.Token = chain.Token
		read.ExpectedSymbol = chain.ExpectSymbol
		res.Raw = append(res.Raw, read.RawCall)
		reads = append(reads, read)
		add("supply.onchain", chain.Name+" JSON-RPC", chain.RPC, map[string]any{
			"token": chain.Token, "symbol": read.Symbol, "decimals": read.Decimals,
			"total_supply": read.Supply, "raw": read.SupplyRaw})
	}

	tick, err := c.ReadBinance(ctx)
	if err != nil {
		return res, fmt.Errorf("spot read: %w", err)
	}
	add("price.binance", "Binance public REST", c.Binance,
		map[string]any{"last_price": tick.LastPrice})

	res.Report = Evaluate(mkt, tick, reads)
	return res, nil
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
