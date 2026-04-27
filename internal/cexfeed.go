package internal

// CexFeed maintains a live mid-price for each token (vs USD) by subscribing to
// Binance bookTicker WebSocket streams. It reconnects automatically on failure.
//
// Usage:
//   feed := NewCexFeed("wss://stream.binance.com:9443/stream", []string{"ETHUSDT","ARBUSDT"})
//   go feed.Run(ctx)
//   price := feed.PriceUSD("WETH")   // 0 until first tick arrives

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// symbolToToken maps Binance symbol prefixes to our internal token symbols.
// Add entries here when new pairs are added to config.
var symbolToToken = map[string]string{
	"ETH":  "WETH",
	"WETH": "WETH",
	"BTC":  "WBTC",
	"WBTC": "WBTC",
	"ARB":  "ARB",
	"LINK": "LINK",
	"GMX":  "GMX",
}

// bookTickerMsg is a Binance combined stream bookTicker payload.
// {"stream":"ethusdt@bookTicker","data":{"u":..,"s":"ETHUSDT","b":"2050.10","B":"1.2","a":"2050.11","A":"0.8"}}
type bookTickerMsg struct {
	Stream string `json:"stream"`
	Data   struct {
		Symbol string `json:"s"`
		Bid    string `json:"b"`
		Ask    string `json:"a"`
	} `json:"data"`
}

// CexFeed holds atomic per-token prices updated by the WebSocket goroutine.
type CexFeed struct {
	wsURL   string
	streams []string // e.g. ["ethusdt@bookTicker","arbusdt@bookTicker"]

	mu     sync.RWMutex
	prices map[string]float64 // token symbol (uppercase) → mid price USD
}

// NewCexFeed constructs a CexFeed. pairs is a list of Binance symbols like "ETHUSDT".
func NewCexFeed(wsBase string, pairs []string) *CexFeed {
	streams := make([]string, len(pairs))
	for i, p := range pairs {
		streams[i] = strings.ToLower(p) + "@bookTicker"
	}
	return &CexFeed{
		wsURL:   wsBase + "?streams=" + strings.Join(streams, "/"),
		streams: streams,
		prices:  make(map[string]float64),
	}
}

// Run connects and maintains the WebSocket subscription. Blocks until ctx is cancelled.
func (f *CexFeed) Run(ctx context.Context) {
	for {
		if err := f.connect(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[cexfeed] disconnected: %v — reconnecting in 5s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (f *CexFeed) connect(ctx context.Context) error {
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, f.wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	log.Printf("[cexfeed] connected (%d streams)", len(f.streams))

	// Read loop
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg bookTickerMsg
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			f.handleTick(msg)
		}
	}()

	select {
	case <-ctx.Done():
		conn.Close()
		<-done
		return nil
	case <-done:
		return fmt.Errorf("cexfeed: connection closed")
	}
}

func (f *CexFeed) handleTick(msg bookTickerMsg) {
	sym := strings.ToUpper(msg.Data.Symbol)
	bid := parseFloat(msg.Data.Bid)
	ask := parseFloat(msg.Data.Ask)
	if bid <= 0 || ask <= 0 {
		return
	}
	mid := (bid + ask) / 2.0

	// Determine quote currency multiplier (USDT/USDC ≈ $1.00)
	// All pairs here are vs USDT which we treat as USD.
	// Strip trailing "USDT" or "USDC" to get base asset.
	base := sym
	for _, suffix := range []string{"USDT", "USDC", "BUSD"} {
		if strings.HasSuffix(sym, suffix) {
			base = strings.TrimSuffix(sym, suffix)
			break
		}
	}

	token, ok := symbolToToken[base]
	if !ok {
		token = base // fallback: use base as-is
	}

	f.mu.Lock()
	f.prices[token] = mid
	f.mu.Unlock()
}

// PriceUSD returns the last known mid price for the given token symbol (e.g. "WETH").
// Returns 0 if no price is available yet.
func (f *CexFeed) PriceUSD(tokenSymbol string) float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.prices[strings.ToUpper(tokenSymbol)]
}

// TokenPriceByAddress returns the USD price for the token at the given address,
// looking it up via the TokenRegistry to resolve symbol.
func (f *CexFeed) TokenPriceByAddress(addr string, tokens *TokenRegistry) float64 {
	t, ok := tokens.Get(addr)
	if !ok {
		return 0
	}
	return f.PriceUSD(t.Symbol)
}

// IsReady returns true once at least one price has been received.
func (f *CexFeed) IsReady() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.prices) > 0
}

// AllPrices returns a snapshot of all current prices (for logging).
func (f *CexFeed) AllPrices() map[string]float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]float64, len(f.prices))
	for k, v := range f.prices {
		out[k] = v
	}
	return out
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v := 0.0
	fmt.Sscanf(s, "%f", &v)
	return v
}

// Mid-price deviation in BPS between DEX effective price and CEX fair value.
// Positive = DEX is more expensive than CEX (sell opportunity).
func SpreadBps(dexPrice, cexPrice float64) float64 {
	if cexPrice <= 0 {
		return 0
	}
	return ((dexPrice - cexPrice) / cexPrice) * 10_000
}

// logSpread returns math.Log(dexPrice/cexPrice) — same unit as log-profit in the cycle cache.
func logSpread(dexPrice, cexPrice float64) float64 {
	if cexPrice <= 0 || dexPrice <= 0 {
		return 0
	}
	return math.Log(dexPrice / cexPrice)
}
