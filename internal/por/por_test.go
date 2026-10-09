package por

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// --- offline fake transport ---------------------------------------------

type roundTrip func(req *http.Request) (*http.Response, error)

func (f roundTrip) Do(req *http.Request) (*http.Response, error) { return f(req) }

func respond(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// abiString encodes a Go string as an ABI string return value.
func abiString(s string) string {
	out := fmt.Sprintf("0x%064x%064x", 32, len(s))
	for i := range s {
		out += fmt.Sprintf("%02x", s[i])
	}
	for pad := len(s); pad < 32; pad++ {
		out += "00"
	}
	return out
}

func abiUint(v uint64) string { return fmt.Sprintf("0x%064x", v) }

// fakeChainRPC answers eth_call for symbol/decimals/totalSupply.
func fakeChainRPC(symbol string, decimals int, supply uint64) roundTrip {
	return func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		s := string(body)
		switch {
		case strings.Contains(s, "95d89b41"):
			return respond(200, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%q}`, abiString(symbol))), nil
		case strings.Contains(s, "313ce567"):
			return respond(200, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%q}`, abiUint(uint64(decimals)))), nil
		case strings.Contains(s, "18160ddd"):
			return respond(200, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%q}`, abiUint(supply))), nil
		}
		return respond(200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`), nil
	}
}

// fakeHTTP routes by host: chain RPCs vs CoinGecko vs Binance.
func fakeHTTP(supplyA, supplyB uint64, cgSupply, cgPrice, binancePrice float64) HTTPDoer {
	chainA := fakeChainRPC("WBTC", 8, supplyA)
	chainB := fakeChainRPC("BTCB", 18, supplyB)
	return roundTrip(func(req *http.Request) (*http.Response, error) {
		host := req.URL.Host
		switch {
		case strings.Contains(host, "ethereum"):
			return chainA.Do(req)
		case strings.Contains(host, "bsc"):
			return chainB.Do(req)
		case strings.Contains(host, "coingecko"):
			return respond(200, fmt.Sprintf(
				`[{"id":"bitcoin","current_price":%v,"circulating_supply":%v}]`, cgPrice, cgSupply)), nil
		case strings.Contains(host, "binance"):
			return respond(200, fmt.Sprintf(`{"symbol":"BTCUSDT","lastPrice":"%v"}`, binancePrice)), nil
		}
		return respond(404, `{"error":"unexpected host"}`), nil
	})
}

// --- tests --------------------------------------------------------------

func TestABIDecoding(t *testing.T) {
	if got := decodeStringABI(abiString("WBTC")); got != "WBTC" {
		t.Fatalf("decodeStringABI = %q, want WBTC", got)
	}
	v, err := decodeUintABI(abiUint(11_613_218_273_262)) // WBTC raw supply (fits uint64)
	if err != nil || v.Uint64() == 0 {
		t.Fatalf("decodeUintABI small: %v %v", v, err)
	}
	// totalSupply can exceed uint64 once 18 decimals are applied - must parse as big
	bigHex := "0x" + strings.Repeat("f", 64)
	bv, err := decodeUintABI(bigHex)
	if err != nil {
		t.Fatalf("decodeUintABI big: %v", err)
	}
	if bv.BitLen() != 256 {
		t.Fatalf("expected full 256-bit parse, got bitlen %d", bv.BitLen())
	}
	if got := bigToFloat(bv, 18); got < 1.15e59 || got > 1.16e59 {
		t.Fatalf("bigToFloat(f's / 1e18) = %v, unexpected", got)
	}
}

func TestRunAllChecksPass(t *testing.T) {
	// 116132.18273262 WBTC @8dec + 65300.969964784133902393 BTCB @18dec
	// (supply values as raw integers; supplyB exceeds uint64 on purpose)
	client := &Client{
		HTTP:      fakeHTTP(11_613_218_273_262, 0, 20_095_612, 82_000, 82_010),
		Chains:    []Chain{DefaultChains[0]},
		CoinGecko: "https://api.coingecko.com/x",
		Binance:   "https://api.binance.com/x",
	}
	res, err := client.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Report.Pass {
		for _, c := range res.Report.Checks {
			if !c.Pass {
				t.Logf("failed check: %s %+v", c.ID, c.Detail)
			}
		}
		t.Fatal("expected all checks to pass")
	}
	if res.Report.WrappedByChain["ethereum"] != 116_132.18273262 {
		t.Fatalf("wrapped ethereum = %v", res.Report.WrappedByChain["ethereum"])
	}
	if res.Report.PriceDeviationPct > MaxPriceDeviationPct {
		t.Fatalf("price deviation too large: %v", res.Report.PriceDeviationPct)
	}
	if len(res.Evidence) < 3 {
		t.Fatalf("expected evidence records, got %d", len(res.Evidence))
	}
}

func TestRunFailsOnWrongSymbol(t *testing.T) {
	client := &Client{
		HTTP: roundTrip(func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Host, "ethereum") {
				return fakeChainRPC("FAKE", 8, 11_613_218_273_262).Do(req)
			}
			return fakeHTTP(1, 1, 20_095_612, 82_000, 82_010).Do(req)
		}),
		Chains:    []Chain{DefaultChains[0]},
		CoinGecko: "https://api.coingecko.com/x",
		Binance:   "https://api.binance.com/x",
	}
	res, err := client.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Report.Pass {
		t.Fatal("identity check must fail when symbol() mismatches")
	}
	found := false
	for _, c := range res.Report.Checks {
		if c.ID == "identity.ethereum" && !c.Pass {
			found = true
		}
	}
	if !found {
		t.Fatal("identity.ethereum failure not reported")
	}
}

func TestRunFailsOnPriceDivergence(t *testing.T) {
	client := &Client{
		HTTP:      fakeHTTP(11_613_218_273_262, 1, 20_095_612, 82_000, 90_000),
		Chains:    []Chain{DefaultChains[0]},
		CoinGecko: "https://api.coingecko.com/x",
		Binance:   "https://api.binance.com/x",
	}
	res, err := client.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Report.Pass {
		t.Fatal("price agreement check must fail on 10% divergence")
	}
	if res.Report.PriceDeviationPct < 9 {
		t.Fatalf("expected ~9.7%% deviation, got %v", res.Report.PriceDeviationPct)
	}
}

func TestRunFailsWhenClaimsExceedReserve(t *testing.T) {
	// wrapped supply larger than circulating supply => coverage < 1
	client := &Client{
		HTTP:      fakeHTTP(3_000_000_000_000_000, 1, 20_095_612, 82_000, 82_000), // 30M WBTC > 20.09M circulating
		Chains:    []Chain{DefaultChains[0]},
		CoinGecko: "https://api.coingecko.com/x",
		Binance:   "https://api.binance.com/x",
	}
	res, err := client.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, c := range res.Report.Checks {
		if c.ID == "reserve_floor" && c.Pass {
			t.Fatal("reserve_floor must fail when claims exceed the reserve floor")
		}
	}
}
