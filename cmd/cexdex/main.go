// cexdex: split-route directional arbitrage bot.
//
// Detects opportunities where a token's aggregate DEX price deviates from the
// Binance mid-price (CEX reference). Executes via SplitArb.sol:
//
//	flash-borrow stablecoin → buy tradeToken on cheapest pool →
//	split-sell across N expensive pools → repay → keep profit
//
// Shares config.yaml and the internal package with the main arb bot.
// Run alongside the main bot — they operate independently.
package main

import (
	"context"
	"log"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"arb-bot/internal"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"gopkg.in/yaml.v3"
)

func main() {
	cfgPath := "config.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}
	cfgAbs, err := filepath.Abs(cfgPath)
	if err != nil {
		log.Fatalf("config abs path: %v", err)
	}
	if err := os.Chdir(filepath.Dir(cfgAbs)); err != nil {
		log.Fatalf("chdir: %v", err)
	}
	cfgPath = filepath.Base(cfgAbs)

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	var cfg internal.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}

	cd := cfg.CexDex
	if !cd.Enabled {
		log.Fatal("[cexdex] disabled in config (cex_dex.enabled: false)")
	}

	privKey := os.Getenv("ARB_BOT_PRIVKEY")

	// ── Connect to Arbitrum node ─────────────────────────────────────────────
	client, err := ethclient.Dial(cfg.ArbitrumRPC)
	if err != nil {
		log.Fatalf("dial node: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── Token + pool registries ──────────────────────────────────────────────
	tokens := internal.NewTokenRegistry()
	for _, t := range cfg.Tokens {
		tokens.Add(internal.NewToken(t.Address, t.Symbol, t.Decimals))
	}

	registry := internal.NewPoolRegistry(
		internal.MinTVLFilter(cfg.Pools.MinTVLUSD),
		internal.MinVolumeFilter(cfg.Pools.MinVolume24h),
		internal.MaxTVLFilter(cfg.Pools.MaxTVLUSD),
		internal.WhitelistFilter(tokens),
	)

	// ── Seed pools (subgraph + config seeds) ─────────────────────────────────
	log.Println("[cexdex] seeding pools...")
	bootstrapPools(ctx, client, &cfg, registry, tokens)

	// ── Continuous multicall refresh ─────────────────────────────────────────
	go func() {
		for {
			pools := registry.All()
			if len(pools) > 0 {
				bCtx, bCancel := context.WithTimeout(ctx, 30*time.Second)
				if err := internal.BatchUpdatePools(bCtx, client, pools); err != nil {
					log.Printf("[cexdex] multicall error: %v", err)
				}
				bCancel()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()

	// ── CEX price feed ───────────────────────────────────────────────────────
	wsBase := cd.BinanceWS
	if wsBase == "" {
		wsBase = "wss://stream.binance.com:9443/stream"
	}
	pairs := cd.Pairs
	if len(pairs) == 0 {
		pairs = []string{"ETHUSDT", "ARBUSDT", "BTCUSDT"}
	}
	cex := internal.NewCexFeed(wsBase, pairs)
	go cex.Run(ctx)
	log.Printf("[cexdex] CEX feed started, pairs=%v", pairs)

	// ── Split executor ───────────────────────────────────────────────────────
	var exec *internal.SplitExecutor
	if privKey != "" && cd.ExecutorContract != "" {
		chainID, err := client.ChainID(ctx)
		if err != nil {
			log.Fatalf("chain id: %v", err)
		}
		exec, err = internal.NewSplitExecutor(client, privKey, cd.ExecutorContract, chainID, cfg.Trading.SequencerRPC)
		if err != nil {
			log.Fatalf("split executor: %v", err)
		}
		log.Printf("[cexdex] executor ready, contract=%s", cd.ExecutorContract)
	} else {
		log.Println("[cexdex] no executor configured — monitor-only mode")
	}

	// ── Splitter ─────────────────────────────────────────────────────────────
	gasCostUSD := cd.GasCostUSD
	if gasCostUSD <= 0 {
		gasCostUSD = 0.50
	}
	minSpreadBps := cd.MinSpreadBps
	if minSpreadBps <= 0 {
		minSpreadBps = 15
	}
	maxTradeUSD := cd.MaxTradeUSD
	if maxTradeUSD <= 0 {
		maxTradeUSD = 50_000
	}
	maxSellHops := cd.MaxSellHops
	if maxSellHops <= 0 {
		maxSellHops = 4
	}
	splitter := internal.NewSplitter(registry, tokens, cex, maxSellHops, minSpreadBps, maxTradeUSD, gasCostUSD)

	// ── Signal handling ───────────────────────────────────────────────────────
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("[cexdex] shutdown signal")
		cancel()
	}()

	// ── Main scan loop ────────────────────────────────────────────────────────
	log.Println("[cexdex] waiting for CEX feed and pool state...")
	scanInterval := time.Duration(cd.ScanIntervalMs) * time.Millisecond
	if cd.ScanIntervalMs <= 0 {
		scanInterval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()

	cooldown := time.Duration(cd.CooldownSecs) * time.Second
	if cooldown <= 0 {
		cooldown = 2 * time.Second
	}
	minProfitUSD := cd.MinProfitUSD
	if minProfitUSD <= 0 {
		minProfitUSD = 1.0
	}

	lastSubmit := time.Time{}

	for {
		select {
		case <-ctx.Done():
			log.Println("[cexdex] stopped")
			return
		case <-ticker.C:
		}

		if !cex.IsReady() {
			continue
		}

		opp := splitter.Scan()
		if opp == nil {
			continue
		}

		log.Printf("[cexdex] %s", opp)

		if exec == nil {
			continue
		}
		if time.Since(lastSubmit) < cooldown {
			continue
		}
		if opp.NetProfitUSD < minProfitUSD {
			continue
		}

		txHash, err := exec.Submit(ctx, opp, 50)
		if err != nil {
			log.Printf("[cexdex] submit failed: %v", err)
			continue
		}
		lastSubmit = time.Now()
		log.Printf("[cexdex] submitted tx=%s profit=$%.2f spread=%.1fbps",
			txHash, opp.NetProfitUSD, opp.SpreadBps)

		go func(hash string, profit float64) {
			rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer rcancel()
			receipt, err := waitForReceipt(rctx, client, hash)
			if err != nil {
				log.Printf("[cexdex] receipt err tx=%s: %v", hash, err)
				return
			}
			if receipt.Status == 1 {
				gasPrice, _ := new(big.Float).SetInt(receipt.EffectiveGasPrice).Float64()
				gasCostETH := float64(receipt.GasUsed) * gasPrice / 1e18
				log.Printf("[cexdex] ✓ tx=%s gasUsed=%d gasCostETH=%.5f estimatedProfit=$%.2f",
					hash, receipt.GasUsed, gasCostETH, profit)
			} else {
				log.Printf("[cexdex] ✗ tx=%s reverted", hash)
			}
		}(txHash, opp.NetProfitUSD)
	}
}

// bootstrapPools seeds the registry from subgraph + config, mirroring the main bot.
func bootstrapPools(ctx context.Context, client *ethclient.Client, cfg *internal.Config,
	registry *internal.PoolRegistry, tokens *internal.TokenRegistry) {

	if cfg.Pools.Subgraph.Enabled {
		sCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		pools := internal.FetchAllSubgraphPools(sCtx, cfg.Pools.Subgraph, tokens)
		cancel()
		n := 0
		for _, p := range pools {
			if registry.ForceAdd(p) {
				n++
			}
		}
		log.Printf("[cexdex] subgraph seeded %d pools", n)
	}

	// Pinned pools: resolve each address from chain (factory→DEX/tokens/fee).
	// Replaces the legacy `seeds:` mechanism. cexdex doesn't have a DB so we
	// use ResolvePoolFromChain directly without persistence.
	n := 0
	for _, addr := range cfg.Pools.PinnedPools {
		rCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		p := internal.ResolvePoolFromChain(rCtx, client, registry, tokens, addr)
		cancel()
		if p == nil {
			continue
		}
		if registry.ForceAdd(p) {
			n++
		}
	}
	log.Printf("[cexdex] resolved %d pinned pools, total: %d", n, registry.Len())
}

// waitForReceipt polls until the transaction is mined or ctx expires.
func waitForReceipt(ctx context.Context, client *ethclient.Client, txHash string) (*types.Receipt, error) {
	hash := common.HexToHash(txHash)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		r, err := client.TransactionReceipt(ctx, hash)
		if err != nil {
			continue // not mined yet
		}
		return r, nil
	}
}
