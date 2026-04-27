package internal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const subgraphTimeout = 20 * time.Second

// SanityMaxTVLUSD caps subgraph-reported TVL/volume values. Anything above is
// treated as broken upstream data (typically a busted price feed for an exotic
// token) and clamped to 0. Set via config: pools.sanity_max_tvl_usd.
var SanityMaxTVLUSD float64 = 1e12 // $1 trillion default — clearly garbage above this

// SubgraphSeedConfig holds subgraph IDs and parameters for the pool seeder.
// Uses The Graph decentralised network — requires a free API key from thegraph.com/studio/apikeys
// (100k queries/month free, more than enough for 4-hour refresh cycles).
type SubgraphSeedConfig struct {
	Enabled bool `yaml:"enabled"`

	// Free API key from https://thegraph.com/studio/apikeys
	TheGraphAPIKey string `yaml:"thegraph_api_key"`

	// Subgraph IDs on The Graph decentralised network.
	// Note: V4 metrics are NOT served by a subgraph — they come from Uniswap's
	// own GraphQL gateway in refreshUniV4Metrics, gated on the top-level
	// `uniswap_api_key` config field.
	UniswapV3SubgraphID  string `yaml:"uniswap_v3_subgraph_id"`
	CamelotV2SubgraphID  string `yaml:"camelot_v2_subgraph_id"`
	CamelotV3SubgraphID  string `yaml:"camelot_v3_subgraph_id"`
	SushiV2SubgraphID    string `yaml:"sushiswap_v2_subgraph_id"`
	BalancerV2SubgraphID string `yaml:"balancer_v2_subgraph_id"`
	CurveSubgraphID      string `yaml:"curve_subgraph_id"`
	RamsesV2SubgraphID   string `yaml:"ramses_v2_subgraph_id"`
	RamsesV3SubgraphID   string `yaml:"ramses_v3_subgraph_id"`
	PancakeV3SubgraphID  string `yaml:"pancakeswap_v3_subgraph_id"` // full Studio URL or subgraph ID
	ChronosSubgraphID    string `yaml:"chronos_subgraph_id"`
	ZyberV3SubgraphID    string `yaml:"zyberswap_v3_subgraph_id"`

	MinTVLUSD    float64 `yaml:"min_tvl_usd"`   // minimum pool TVL to seed (e.g. 200000)
	LimitPerDEX  int     `yaml:"limit_per_dex"` // pools to fetch per DEX (e.g. 50)
	RefreshHours int     `yaml:"refresh_hours"` // how often to refresh TVL/vol metrics
}

// gatewayURL builds the The Graph gateway URL for a given subgraph ID and API key.
func (c SubgraphSeedConfig) gatewayURL(subgraphID string) string {
	if subgraphID == "" || c.TheGraphAPIKey == "" {
		return ""
	}
	// If already a full URL (Studio endpoints, etc.), return as-is.
	if strings.HasPrefix(subgraphID, "https://") {
		return subgraphID
	}
	return fmt.Sprintf("https://gateway.thegraph.com/api/%s/subgraphs/id/%s",
		c.TheGraphAPIKey, subgraphID)
}

// ── GraphQL transport ─────────────────────────────────────────────────────────

type gqlRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

func graphqlPost(ctx context.Context, url string, req gqlRequest, dst interface{}) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, subgraphTimeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(rctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		preview := string(raw)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, preview)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("subgraph error: %s", envelope.Errors[0].Message)
	}
	return json.Unmarshal(envelope.Data, dst)
}

// ── Subgraph token / pool types ───────────────────────────────────────────────

type sgToken struct {
	ID       string `json:"id"`
	Symbol   string `json:"symbol"`
	Decimals string `json:"decimals"`
}

type sgV3Pool struct {
	ID                  string  `json:"id"`
	Token0              sgToken `json:"token0"`
	Token1              sgToken `json:"token1"`
	FeeTier             string  `json:"feeTier"`
	TotalValueLockedUSD string  `json:"totalValueLockedUSD"`
	VolumeUSD           string  `json:"volumeUSD"` // cumulative; we divide by 30 for daily estimate
}

type sgV2Pair struct {
	ID         string  `json:"id"`
	Token0     sgToken `json:"token0"`
	Token1     sgToken `json:"token1"`
	ReserveUSD string  `json:"reserveUSD"`
	VolumeUSD  string  `json:"volumeUSD"`
}

// ── Fetchers ──────────────────────────────────────────────────────────────────

// FetchV3SubgraphPools queries a UniswapV3-compatible subgraph for the top pools by TVL.
func FetchV3SubgraphPools(ctx context.Context, url string, dex DEXType, minTVL float64, limit int, tokens *TokenRegistry) ([]*Pool, error) {
	const q = `
	query($minTVL: String!, $limit: Int!) {
		pools(
			first: $limit
			orderBy: totalValueLockedUSD
			orderDirection: desc
			where: { totalValueLockedUSD_gt: $minTVL }
		) {
			id
			token0 { id symbol decimals }
			token1 { id symbol decimals }
			feeTier
			totalValueLockedUSD
			volumeUSD
		}
	}`

	var data struct {
		Pools []sgV3Pool `json:"pools"`
	}
	err := graphqlPost(ctx, url, gqlRequest{
		Query: q,
		Variables: map[string]interface{}{
			"minTVL": fmt.Sprintf("%.0f", minTVL),
			"limit":  limit,
		},
	}, &data)
	if err != nil {
		return nil, err
	}

	out := make([]*Pool, 0, len(data.Pools))
	for _, sp := range data.Pools {
		t0 := resolveSubgraphToken(sp.Token0, tokens)
		t1 := resolveSubgraphToken(sp.Token1, tokens)
		fee, _ := strconv.ParseUint(sp.FeeTier, 10, 32)
		tvl, _ := strconv.ParseFloat(sp.TotalValueLockedUSD, 64)
		out = append(out, &Pool{
			Address: strings.ToLower(sp.ID),
			DEX:     dex,
			// Subgraph returns the fee in pips (parts-per-million). Store BOTH:
			//   - FeePPM is the lossless authoritative value (used by v3Fee for
			//     the on-chain executor calldata; precision matters for sub-1bps
			//     tiers like RamsesV3 fee=1 = 0.0001%, where bps would round to 0)
			//   - FeeBps is the convenience approximation used by graph weights
			FeePPM:  uint32(fee),
			FeeBps:  uint32(fee / 100),
			Token0:  t0,
			Token1:  t1,
			TVLUSD:  tvl,
			// Volume24h starts at 0 — populated by the metrics refresh which queries
			// poolDayData/dailySnapshots for actual 24h volume (not all-time/30).
			Volume24hUSD: 0,
		})
	}
	return out, nil
}

// FetchMessariLiquidityPools queries a Messari-standard DEX subgraph (used by SushiSwap, etc.)
// for the top pools by TVL. These use `liquidityPools` with `inputTokens` instead of token0/token1.
// Note: Messari schema returns token decimals as an integer, not a string.
func FetchMessariLiquidityPools(ctx context.Context, url string, dex DEXType, defaultFeeBps uint32, minTVL float64, limit int, tokens *TokenRegistry) ([]*Pool, error) {
	const q = `
	query($minTVL: String!, $limit: Int!) {
		liquidityPools(
			first: $limit
			orderBy: totalValueLockedUSD
			orderDirection: desc
			where: { totalValueLockedUSD_gt: $minTVL }
		) {
			id
			inputTokens { id symbol decimals }
			totalValueLockedUSD
			cumulativeVolumeUSD
		}
	}`

	// Messari schema returns decimals as a JSON integer, not a string.
	type messariToken struct {
		ID       string `json:"id"`
		Symbol   string `json:"symbol"`
		Decimals int    `json:"decimals"`
	}
	var data struct {
		LiquidityPools []struct {
			ID                  string         `json:"id"`
			InputTokens         []messariToken `json:"inputTokens"`
			TotalValueLockedUSD string         `json:"totalValueLockedUSD"`
			CumulativeVolumeUSD string         `json:"cumulativeVolumeUSD"`
		} `json:"liquidityPools"`
	}
	err := graphqlPost(ctx, url, gqlRequest{
		Query: q,
		Variables: map[string]interface{}{
			"minTVL": fmt.Sprintf("%.0f", minTVL),
			"limit":  limit,
		},
	}, &data)
	if err != nil {
		return nil, err
	}

	out := make([]*Pool, 0, len(data.LiquidityPools))
	for _, sp := range data.LiquidityPools {
		if len(sp.InputTokens) < 2 {
			continue
		}
		mt0, mt1 := sp.InputTokens[0], sp.InputTokens[1]
		t0 := resolveSubgraphToken(sgToken{ID: mt0.ID, Symbol: mt0.Symbol, Decimals: strconv.Itoa(mt0.Decimals)}, tokens)
		t1 := resolveSubgraphToken(sgToken{ID: mt1.ID, Symbol: mt1.Symbol, Decimals: strconv.Itoa(mt1.Decimals)}, tokens)
		tvl, _ := strconv.ParseFloat(sp.TotalValueLockedUSD, 64)
		out = append(out, &Pool{
			Address:      strings.ToLower(sp.ID),
			DEX:          dex,
			FeeBps:       defaultFeeBps,
			Token0:       t0,
			Token1:       t1,
			TVLUSD:       tvl,
			Volume24hUSD: 0,
		})
	}
	return out, nil
}

// FetchV2SubgraphPairs queries a UniswapV2-compatible subgraph for the top pairs by TVL.
func FetchV2SubgraphPairs(ctx context.Context, url string, dex DEXType, defaultFeeBps uint32, minTVL float64, limit int, tokens *TokenRegistry) ([]*Pool, error) {
	const q = `
	query($minTVL: String!, $limit: Int!) {
		pairs(
			first: $limit
			orderBy: reserveUSD
			orderDirection: desc
			where: { reserveUSD_gt: $minTVL }
		) {
			id
			token0 { id symbol decimals }
			token1 { id symbol decimals }
			reserveUSD
			volumeUSD
		}
	}`

	var data struct {
		Pairs []sgV2Pair `json:"pairs"`
	}
	err := graphqlPost(ctx, url, gqlRequest{
		Query: q,
		Variables: map[string]interface{}{
			"minTVL": fmt.Sprintf("%.0f", minTVL),
			"limit":  limit,
		},
	}, &data)
	if err != nil {
		return nil, err
	}

	out := make([]*Pool, 0, len(data.Pairs))
	for _, sp := range data.Pairs {
		t0 := resolveSubgraphToken(sp.Token0, tokens)
		t1 := resolveSubgraphToken(sp.Token1, tokens)
		tvl, _ := strconv.ParseFloat(sp.ReserveUSD, 64)
		out = append(out, &Pool{
			Address:      strings.ToLower(sp.ID),
			DEX:          dex,
			FeeBps:       defaultFeeBps,
			Token0:       t0,
			Token1:       t1,
			TVLUSD:       tvl,
			Volume24hUSD: 0,
		})
	}
	return out, nil
}

// FetchSolidlyPairs queries a Solidly-fork subgraph (Ramses, Chronos, etc).
// These use `pools` (not `pairs`) and `totalValueLockedUSD` (not `reserveUSD`).
func FetchSolidlyPairs(ctx context.Context, url string, dex DEXType, defaultFeeBps uint32, minTVL float64, limit int, tokens *TokenRegistry) ([]*Pool, error) {
	const q = `
	query($minTVL: String!, $limit: Int!) {
		pools(
			first: $limit
			orderBy: totalValueLockedUSD
			orderDirection: desc
			where: { totalValueLockedUSD_gt: $minTVL }
		) {
			id
			token0 { id symbol decimals }
			token1 { id symbol decimals }
			isStable
			totalValueLockedUSD
			volumeUSD
		}
	}`

	var data struct {
		Pools []struct {
			ID                  string  `json:"id"`
			Token0              sgToken `json:"token0"`
			Token1              sgToken `json:"token1"`
			IsStable            bool    `json:"isStable"`
			TotalValueLockedUSD string  `json:"totalValueLockedUSD"`
			VolumeUSD           string  `json:"volumeUSD"`
		} `json:"pools"`
	}
	err := graphqlPost(ctx, url, gqlRequest{
		Query: q,
		Variables: map[string]interface{}{
			"minTVL": fmt.Sprintf("%.0f", minTVL),
			"limit":  limit,
		},
	}, &data)
	if err != nil {
		return nil, err
	}

	out := make([]*Pool, 0, len(data.Pools))
	for _, sp := range data.Pools {
		t0 := resolveSubgraphToken(sp.Token0, tokens)
		t1 := resolveSubgraphToken(sp.Token1, tokens)
		tvl, _ := strconv.ParseFloat(sp.TotalValueLockedUSD, 64)
		feeBps := defaultFeeBps
		if sp.IsStable {
			feeBps = 4 // Solidly stable pools typically charge 0.04%
		}
		out = append(out, &Pool{
			Address:      strings.ToLower(sp.ID),
			DEX:          dex,
			FeeBps:       feeBps,
			IsStable:     sp.IsStable,
			Token0:       t0,
			Token1:       t1,
			TVLUSD:       tvl,
			Volume24hUSD: 0,
		})
	}
	return out, nil
}

// FetchBalancerPools queries the Balancer V2 subgraph for 2-token weighted pools sorted by TVL.
// Pools with more than 2 tokens are skipped — our simulator only handles binary Balancer pools.
// The Balancer pool `id` field is the 32-byte pool ID used by the Vault, not the contract address.
func FetchBalancerPools(ctx context.Context, url string, minTVL float64, limit int, tokens *TokenRegistry) ([]*Pool, error) {
	const q = `
	query($minTVL: String!, $limit: Int!) {
		pools(
			first: $limit
			orderBy: totalLiquidity
			orderDirection: desc
			where: { poolType: "Weighted", totalLiquidity_gt: $minTVL }
		) {
			id
			address
			swapFee
			totalLiquidity
			totalSwapVolume
			tokens { address symbol decimals weight }
		}
	}`

	type balToken struct {
		Address  string `json:"address"`
		Symbol   string `json:"symbol"`
		Decimals int    `json:"decimals"`
		Weight   string `json:"weight"` // decimal, e.g. "0.8"
	}
	var data struct {
		Pools []struct {
			ID              string     `json:"id"`      // bytes32 poolId
			Address         string     `json:"address"` // contract address
			SwapFee         string     `json:"swapFee"` // decimal, e.g. "0.001"
			TotalLiquidity  string     `json:"totalLiquidity"`
			TotalSwapVolume string     `json:"totalSwapVolume"`
			Tokens          []balToken `json:"tokens"`
		} `json:"pools"`
	}
	err := graphqlPost(ctx, url, gqlRequest{
		Query: q,
		Variables: map[string]interface{}{
			"minTVL": fmt.Sprintf("%.0f", minTVL),
			"limit":  limit,
		},
	}, &data)
	if err != nil {
		return nil, err
	}

	out := make([]*Pool, 0, len(data.Pools))
	for _, sp := range data.Pools {
		if len(sp.Tokens) != 2 {
			continue // skip N-token pools
		}
		t0 := resolveSubgraphToken(sgToken{ID: sp.Tokens[0].Address, Symbol: sp.Tokens[0].Symbol, Decimals: strconv.Itoa(sp.Tokens[0].Decimals)}, tokens)
		t1 := resolveSubgraphToken(sgToken{ID: sp.Tokens[1].Address, Symbol: sp.Tokens[1].Symbol, Decimals: strconv.Itoa(sp.Tokens[1].Decimals)}, tokens)
		feeF, _ := strconv.ParseFloat(sp.SwapFee, 64)
		tvl, _ := strconv.ParseFloat(sp.TotalLiquidity, 64)
		w0, _ := strconv.ParseFloat(sp.Tokens[0].Weight, 64)
		w1, _ := strconv.ParseFloat(sp.Tokens[1].Weight, 64)
		out = append(out, &Pool{
			Address:      strings.ToLower(sp.Address),
			DEX:          DEXBalancerWeighted,
			FeeBps:       uint32(math.Round(feeF * 10000)),
			Token0:       t0,
			Token1:       t1,
			PoolID:       strings.ToLower(sp.ID),
			Weight0:      w0,
			Weight1:      w1,
			TVLUSD:       tvl,
			Volume24hUSD: 0,
		})
	}
	return out, nil
}

// FetchAlgebraV3Pools queries an Algebra-based concentrated liquidity subgraph (e.g. Camelot V3).
// Fee is omitted from the query — Algebra fees are dynamic and set per-swap; the live fee
// is read on every block via globalState() in BatchUpdatePools, so starting at 0 is fine.
func FetchAlgebraV3Pools(ctx context.Context, url string, dex DEXType, minTVL float64, limit int, tokens *TokenRegistry) ([]*Pool, error) {
	const q = `
	query($minTVL: String!, $limit: Int!) {
		pools(
			first: $limit
			orderBy: totalValueLockedUSD
			orderDirection: desc
			where: { totalValueLockedUSD_gt: $minTVL }
		) {
			id
			token0 { id symbol decimals }
			token1 { id symbol decimals }
			totalValueLockedUSD
			volumeUSD
		}
	}`

	var data struct {
		Pools []struct {
			ID                  string  `json:"id"`
			Token0              sgToken `json:"token0"`
			Token1              sgToken `json:"token1"`
			TotalValueLockedUSD string  `json:"totalValueLockedUSD"`
			VolumeUSD           string  `json:"volumeUSD"`
		} `json:"pools"`
	}
	err := graphqlPost(ctx, url, gqlRequest{
		Query: q,
		Variables: map[string]interface{}{
			"minTVL": fmt.Sprintf("%.0f", minTVL),
			"limit":  limit,
		},
	}, &data)
	if err != nil {
		return nil, err
	}

	out := make([]*Pool, 0, len(data.Pools))
	for _, sp := range data.Pools {
		t0 := resolveSubgraphToken(sp.Token0, tokens)
		t1 := resolveSubgraphToken(sp.Token1, tokens)
		tvl, _ := strconv.ParseFloat(sp.TotalValueLockedUSD, 64)
		out = append(out, &Pool{
			Address:      strings.ToLower(sp.ID),
			DEX:          dex,
			FeeBps:       0, // updated each block via globalState() multicall
			Token0:       t0,
			Token1:       t1,
			TVLUSD:       tvl,
			Volume24hUSD: 0,
		})
	}
	return out, nil
}

// FetchCurvePools queries a Curve subgraph for 2-coin pools (convex-community schema).
// Pools with more than 2 coins are skipped — our Curve simulator only handles binary pools.
// Note: Curve Arbitrum subgraph availability on The Graph is limited; errors are expected.
func FetchCurvePools(ctx context.Context, url string, minTVL float64, limit int, tokens *TokenRegistry) ([]*Pool, error) {
	const q = `
	query($limit: Int!) {
		pools(
			first: $limit
			orderBy: totalVolume
			orderDirection: desc
			where: { coinCount: 2 }
		) {
			id
			coinCount
			A
			fee
			totalVolume
			coins {
				token { address symbol decimals }
			}
		}
	}`

	type curveToken struct {
		Address  string `json:"address"`
		Symbol   string `json:"symbol"`
		Decimals int    `json:"decimals"`
	}
	var data struct {
		Pools []struct {
			ID          string `json:"id"` // pool address
			CoinCount   int    `json:"coinCount"`
			A           string `json:"A"`
			Fee         string `json:"fee"`         // decimal, e.g. "0.0004"
			TotalVolume string `json:"totalVolume"` // cumulative USD
			Coins       []struct {
				Token curveToken `json:"token"`
			} `json:"coins"`
		} `json:"pools"`
	}
	err := graphqlPost(ctx, url, gqlRequest{
		Query:     q,
		Variables: map[string]interface{}{"limit": limit},
	}, &data)
	if err != nil {
		return nil, err
	}

	out := make([]*Pool, 0, len(data.Pools))
	for _, sp := range data.Pools {
		if len(sp.Coins) != 2 {
			continue
		}
		t0 := resolveSubgraphToken(sgToken{ID: sp.Coins[0].Token.Address, Symbol: sp.Coins[0].Token.Symbol, Decimals: strconv.Itoa(sp.Coins[0].Token.Decimals)}, tokens)
		t1 := resolveSubgraphToken(sgToken{ID: sp.Coins[1].Token.Address, Symbol: sp.Coins[1].Token.Symbol, Decimals: strconv.Itoa(sp.Coins[1].Token.Decimals)}, tokens)
		feeF, _ := strconv.ParseFloat(sp.Fee, 64)
		amp, _ := strconv.ParseUint(sp.A, 10, 64)
		out = append(out, &Pool{
			Address:      strings.ToLower(sp.ID),
			DEX:          DEXCurve,
			IsStable:     true,
			FeeBps:       uint32(math.Round(feeF * 10000)),
			Token0:       t0,
			Token1:       t1,
			AmpFactor:    amp,
			Volume24hUSD: 0,
		})
	}
	return out, nil
}

// FetchAllSubgraphPools fetches from all configured DEXes and returns the combined list.
// Errors per-DEX are logged but do not abort the others.
func FetchAllSubgraphPools(ctx context.Context, cfg SubgraphSeedConfig, tokens *TokenRegistry) []*Pool {
	if !cfg.Enabled {
		return nil
	}
	limit := cfg.LimitPerDEX
	if limit <= 0 {
		limit = 50
	}
	minTVL := cfg.MinTVLUSD
	if minTVL <= 0 {
		minTVL = 200_000
	}

	var all []*Pool

	type fetchMode int
	const (
		modeV3       fetchMode = iota
		modeV2Pairs            // UniV2-compatible: pairs schema
		modeMessari            // Messari standard: liquidityPools schema
		modeAlgebra            // Algebra/CamelotV3: pools with `fee` field
		modeBalancer           // Balancer V2: weighted pools
		modeCurve              // Curve: 2-coin pools
		modeSolidly            // Solidly-fork: pools + totalValueLockedUSD + isStable
	)
	type source struct {
		id     string
		dex    DEXType
		mode   fetchMode
		feeBps uint32
		minTVL float64 // 0 = use global minTVL
		label  string
	}
	sources := []source{
		{cfg.UniswapV3SubgraphID, DEXUniswapV3, modeV3, 0, 0, "UniV3"},
		{cfg.CamelotV2SubgraphID, DEXCamelot, modeV2Pairs, 30, 0, "CamelotV2"},
		{cfg.CamelotV3SubgraphID, DEXCamelotV3, modeAlgebra, 0, 0, "CamelotV3"},
		{cfg.SushiV2SubgraphID, DEXSushiSwap, modeMessari, 30, 0, "SushiV2"},
		{cfg.BalancerV2SubgraphID, DEXBalancerWeighted, modeBalancer, 0, 50_000, "Balancer"},
		{cfg.CurveSubgraphID, DEXCurve, modeCurve, 0, 50_000, "Curve"},
		// RamsesV2: subgraph schema incompatible (neither `pairs` nor `pools` entity found)
		{cfg.RamsesV3SubgraphID, DEXRamsesV3, modeV3, 0, 0, "RamsesV3"},
		{cfg.PancakeV3SubgraphID, DEXPancakeV3, modeV3, 0, 0, "PancakeV3"},
		// SushiV3: no working public subgraph found — covered by config seeds only
		// Chronos: indexers unavailable (abandoned DEX)
		// ZyberV3: subgraph not found (abandoned DEX)
	}

	for _, s := range sources {
		url := cfg.gatewayURL(s.id)
		if url == "" {
			continue
		}
		srcMinTVL := minTVL
		if s.minTVL > 0 {
			srcMinTVL = s.minTVL
		}
		var (
			pools []*Pool
			err   error
		)
		switch s.mode {
		case modeV2Pairs:
			pools, err = FetchV2SubgraphPairs(ctx, url, s.dex, s.feeBps, srcMinTVL, limit, tokens)
		case modeMessari:
			pools, err = FetchMessariLiquidityPools(ctx, url, s.dex, s.feeBps, srcMinTVL, limit, tokens)
		case modeAlgebra:
			pools, err = FetchAlgebraV3Pools(ctx, url, s.dex, srcMinTVL, limit, tokens)
		case modeBalancer:
			pools, err = FetchBalancerPools(ctx, url, srcMinTVL, limit, tokens)
		case modeCurve:
			pools, err = FetchCurvePools(ctx, url, srcMinTVL, limit, tokens)
		case modeSolidly:
			pools, err = FetchSolidlyPairs(ctx, url, s.dex, s.feeBps, srcMinTVL, limit, tokens)
		default:
			pools, err = FetchV3SubgraphPools(ctx, url, s.dex, srcMinTVL, limit, tokens)
		}
		if err != nil {
			log.Printf("[subgraph] %s fetch error: %v", s.label, err)
		} else {
			log.Printf("[subgraph] fetched %d %s pools (TVL≥$%.0fk)", len(pools), s.label, minTVL/1000)
			all = append(all, pools...)
		}
	}

	return all
}

// RefreshPoolMetrics re-fetches TVL and 24h-volume estimates for all pools in the
// registry. V2/V3 forks use The Graph subgraphs (configured per DEX); UniV4 uses
// Uniswap's own GraphQL gateway with the shared `uniswap_api_key`. Only updates
// pools already in the registry — does not add new ones.
func RefreshPoolMetrics(ctx context.Context, cfg SubgraphSeedConfig, uniswapAPIKey string, registry *PoolRegistry, tokens *TokenRegistry) {
	if !cfg.Enabled {
		return
	}

	// Collect addresses split by DEX type so we can query the right subgraph.
	v3addrs := make([]string, 0, 64)
	camelotV2Addrs := make([]string, 0, 64)
	camelotV3Addrs := make([]string, 0, 64)
	sushiAddrs := make([]string, 0, 64)
	balancerAddrs := make([]string, 0, 64)
	curveAddrs := make([]string, 0, 64)
	ramsesV2Addrs := make([]string, 0, 32)
	ramsesV3Addrs := make([]string, 0, 32)
	pancakeV3Addrs := make([]string, 0, 32)
	chronosAddrs := make([]string, 0, 32)
	zyberV3Addrs := make([]string, 0, 32)
	uniV4Ids := make([]string, 0, 64) // V4 keyed by poolId, not address

	for _, p := range registry.All() {
		addr := strings.ToLower(p.Address)
		switch p.DEX {
		case DEXUniswapV3:
			v3addrs = append(v3addrs, addr)
		case DEXUniswapV4:
			uniV4Ids = append(uniV4Ids, addr) // For V4 the Address field IS the poolId
		case DEXCamelot:
			camelotV2Addrs = append(camelotV2Addrs, addr)
		case DEXCamelotV3:
			camelotV3Addrs = append(camelotV3Addrs, addr)
		case DEXSushiSwap:
			sushiAddrs = append(sushiAddrs, addr)
		case DEXBalancerWeighted:
			balancerAddrs = append(balancerAddrs, addr)
		case DEXCurve:
			curveAddrs = append(curveAddrs, addr)
		case DEXRamsesV2:
			ramsesV2Addrs = append(ramsesV2Addrs, addr)
		case DEXRamsesV3:
			ramsesV3Addrs = append(ramsesV3Addrs, addr)
		case DEXPancakeV3:
			pancakeV3Addrs = append(pancakeV3Addrs, addr)
		case DEXChronos:
			chronosAddrs = append(chronosAddrs, addr)
		case DEXZyberV3:
			zyberV3Addrs = append(zyberV3Addrs, addr)
		}
	}

	if url := cfg.gatewayURL(cfg.UniswapV3SubgraphID); url != "" && len(v3addrs) > 0 {
		refreshV3Metrics(ctx, url, v3addrs, registry)
	}
	if url := cfg.gatewayURL(cfg.CamelotV2SubgraphID); url != "" && len(camelotV2Addrs) > 0 {
		refreshV2Metrics(ctx, url, camelotV2Addrs, registry)
	}
	if url := cfg.gatewayURL(cfg.CamelotV3SubgraphID); url != "" && len(camelotV3Addrs) > 0 {
		refreshV3Metrics(ctx, url, camelotV3Addrs, registry)
	}
	if url := cfg.gatewayURL(cfg.SushiV2SubgraphID); url != "" && len(sushiAddrs) > 0 {
		refreshMessariMetrics(ctx, url, sushiAddrs, registry)
	}
	if url := cfg.gatewayURL(cfg.BalancerV2SubgraphID); url != "" && len(balancerAddrs) > 0 {
		refreshBalancerMetrics(ctx, url, balancerAddrs, registry)
	}
	if url := cfg.gatewayURL(cfg.CurveSubgraphID); url != "" && len(curveAddrs) > 0 {
		refreshCurveMetrics(ctx, url, curveAddrs, registry)
	}
	if url := cfg.gatewayURL(cfg.RamsesV2SubgraphID); url != "" && len(ramsesV2Addrs) > 0 {
		refreshV2Metrics(ctx, url, ramsesV2Addrs, registry)
	}
	if url := cfg.gatewayURL(cfg.RamsesV3SubgraphID); url != "" && len(ramsesV3Addrs) > 0 {
		refreshV3Metrics(ctx, url, ramsesV3Addrs, registry)
	}
	if url := cfg.gatewayURL(cfg.PancakeV3SubgraphID); url != "" && len(pancakeV3Addrs) > 0 {
		refreshV3Metrics(ctx, url, pancakeV3Addrs, registry)
	}
	if uniswapAPIKey != "" && len(uniV4Ids) > 0 {
		refreshUniV4Metrics(ctx, uniswapAPIKey, registry)
	}
	// Chronos/ZyberV3: abandoned DEXes — no active subgraph to refresh
	_ = chronosAddrs
	_ = zyberV3Addrs
}

func refreshV3Metrics(ctx context.Context, url string, addrs []string, registry *PoolRegistry) {
	// Query both:
	//   1. pool.totalValueLockedUSD (current TVL)
	//   2. poolDayData (last 2 days, most recent first) for actual 24h volume
	// The Pool entity's volumeUSD field is ALL-TIME cumulative — dividing by 30
	// produces nonsense (huge for old pools, 0 for inactive ones). Day data gives
	// the true rolling 24h volume.
	//
	// `first: 1000` is the max page size on The Graph. We pass it explicitly because
	// the default is 100 — without this, only the first 100 pools matching the
	// id_in filter come back, silently dropping the rest.
	const q = `
	query($ids: [String!]!) {
		pools(first: 1000, where: { id_in: $ids }) {
			id
			totalValueLockedUSD
			poolDayData(first: 2, orderBy: date, orderDirection: desc) {
				date
				volumeUSD
			}
		}
	}`
	var data struct {
		Pools []struct {
			ID                  string `json:"id"`
			TotalValueLockedUSD string `json:"totalValueLockedUSD"`
			PoolDayData         []struct {
				Date      int64  `json:"date"`
				VolumeUSD string `json:"volumeUSD"`
			} `json:"poolDayData"`
		} `json:"pools"`
	}
	if err := graphqlPost(ctx, url, gqlRequest{
		Query:     q,
		Variables: map[string]interface{}{"ids": addrs},
	}, &data); err != nil {
		log.Printf("[subgraph] v3 metrics refresh error: %v", err)
		return
	}
	updated := 0
	for _, sp := range data.Pools {
		p, ok := registry.Get(sp.ID)
		if !ok {
			continue
		}
		tvl, _ := strconv.ParseFloat(sp.TotalValueLockedUSD, 64)
		// Pick the most recent complete day. If today's data exists but is partial,
		// fall back to yesterday's full day to get a stable 24h figure.
		var vol float64
		if len(sp.PoolDayData) > 0 {
			vol, _ = strconv.ParseFloat(sp.PoolDayData[0].VolumeUSD, 64)
		}
		if tvl > SanityMaxTVLUSD {
			tvl = 0
		}
		if vol > SanityMaxTVLUSD {
			vol = 0
		}
		p.mu.Lock()
		p.TVLUSD = tvl
		p.Volume24hUSD = vol
		p.mu.Unlock()
		updated++
	}
	log.Printf("[subgraph] refreshed metrics for %d V3 pools", updated)
}

// refreshUniV4Metrics fetches TVL and 24h volume for the top V4 pools on
// Arbitrum using Uniswap's own GraphQL gateway (https://interface.gateway.uniswap.org).
// This avoids needing a separate V4 subgraph — Uniswap's API exposes both
// `totalLiquidity { value }` (USD TVL) and `cumulativeVolume(duration: DAY) { value }`
// in a single query for all 100 top pools. Authentication uses the same
// `uniswap_api_key` already configured for arbscan's V4 pool discovery.
//
// Pool ID format: the API returns base64-encoded "V4Pool:ARBITRUM_0x{poolId}"
// strings — we decode to extract the bytes32 poolId for registry lookup.
//
// Note: this writes to BOTH the in-memory pool AND directly to v4_pools via
// the dashboard's read path, since V4 lives in its own table. The bot's main
// loop persists the in-memory state on the next multicall iteration.
func refreshUniV4Metrics(ctx context.Context, apiKey string, registry *PoolRegistry) {
	const q = `{"query":"{ topV4Pools(first: 100, chain: ARBITRUM) { id totalLiquidity { value } cumulativeVolume(duration: DAY) { value } } }"}`

	rctx, cancel := context.WithTimeout(ctx, subgraphTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost,
		"https://interface.gateway.uniswap.org/v1/graphql", strings.NewReader(q))
	if err != nil {
		log.Printf("[uniV4] build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Origin", "https://app.uniswap.org")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[uniV4] fetch: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[uniV4] fetch: HTTP %d", resp.StatusCode)
		return
	}
	var result struct {
		Data struct {
			TopV4Pools []struct {
				ID              string `json:"id"`
				TotalLiquidity  *struct {
					Value float64 `json:"value"`
				} `json:"totalLiquidity"`
				CumulativeVolume *struct {
					Value float64 `json:"value"`
				} `json:"cumulativeVolume"`
			} `json:"topV4Pools"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[uniV4] decode: %v", err)
		return
	}

	updated := 0
	for _, p := range result.Data.TopV4Pools {
		// Decode base64 id → "V4Pool:ARBITRUM_0x{poolId}"
		raw, err := base64.StdEncoding.DecodeString(p.ID)
		if err != nil {
			continue
		}
		parts := strings.SplitN(string(raw), "_", 2)
		if len(parts) != 2 {
			continue
		}
		poolID := strings.ToLower(parts[1])
		pool, ok := registry.Get(poolID)
		if !ok {
			continue
		}
		var tvl, vol float64
		if p.TotalLiquidity != nil {
			tvl = p.TotalLiquidity.Value
		}
		if p.CumulativeVolume != nil {
			vol = p.CumulativeVolume.Value
		}
		if tvl > SanityMaxTVLUSD {
			tvl = 0
		}
		if vol > SanityMaxTVLUSD {
			vol = 0
		}
		pool.mu.Lock()
		// Only overwrite TVL if we got a real value — leave the on-chain
		// recomputeV3TVL result as fallback for pools missing from the API.
		if tvl > 0 {
			pool.TVLUSD = tvl
		}
		pool.Volume24hUSD = vol
		pool.mu.Unlock()
		updated++
	}
	log.Printf("[uniV4] refreshed metrics for %d UniV4 pools (TVL+volume from Uniswap API)", updated)
}

func refreshV2Metrics(ctx context.Context, url string, addrs []string, registry *PoolRegistry) {
	// V2 schemas vary across forks: standard Uniswap V2 nests `pairDayData` under
	// the Pair entity, but Camelot's V2 fork only exposes top-level `pairDayDatas`
	// keyed by `pairAddress`. We query both shapes in a single GraphQL request and
	// take whichever returns data.
	//
	// TVL comes from the `pairs` query (reserveUSD); volume comes from whichever
	// pairDayData* path the indexer supports.
	//
	// `first: 1000` overrides the default 100-row page size on The Graph.
	const q = `
	query($ids: [String!]!) {
		pairs(first: 1000, where: { id_in: $ids }) {
			id
			reserveUSD
		}
		pairDayDatas(first: 1000, orderBy: date, orderDirection: desc, where: { pairAddress_in: $ids }) {
			date
			dailyVolumeUSD
			pairAddress
		}
	}`
	var data struct {
		Pairs []struct {
			ID         string `json:"id"`
			ReserveUSD string `json:"reserveUSD"`
		} `json:"pairs"`
		PairDayDatas []struct {
			Date           int64  `json:"date"`
			DailyVolumeUSD string `json:"dailyVolumeUSD"`
			PairAddress    string `json:"pairAddress"`
		} `json:"pairDayDatas"`
	}
	if err := graphqlPost(ctx, url, gqlRequest{
		Query:     q,
		Variables: map[string]interface{}{"ids": addrs},
	}, &data); err != nil {
		log.Printf("[subgraph] v2 metrics refresh error: %v", err)
		return
	}

	// Build a per-pair latest-day-volume map. Multiple day rows per pair come back
	// in date-desc order, so the first one we see for each pair is the most recent.
	latestVol := make(map[string]float64, len(data.PairDayDatas))
	for _, d := range data.PairDayDatas {
		addr := strings.ToLower(d.PairAddress)
		if _, seen := latestVol[addr]; seen {
			continue
		}
		v, _ := strconv.ParseFloat(d.DailyVolumeUSD, 64)
		latestVol[addr] = v
	}

	updated := 0
	for _, sp := range data.Pairs {
		p, ok := registry.Get(sp.ID)
		if !ok {
			continue
		}
		tvl, _ := strconv.ParseFloat(sp.ReserveUSD, 64)
		vol := latestVol[strings.ToLower(sp.ID)]
		if tvl > SanityMaxTVLUSD {
			tvl = 0
		}
		if vol > SanityMaxTVLUSD {
			vol = 0
		}
		p.mu.Lock()
		p.TVLUSD = tvl
		p.Volume24hUSD = vol
		p.mu.Unlock()
		updated++
	}
	log.Printf("[subgraph] refreshed metrics for %d V2 pools", updated)
}

func refreshMessariMetrics(ctx context.Context, url string, addrs []string, registry *PoolRegistry) {
	// Messari schema: liquidityPoolDailySnapshots[0].dailyVolumeUSD = actual 24h volume.
	// cumulativeVolumeUSD is all-time and useless for daily metrics.
	// Note: `timestamp` is a BigInt in the Messari schema (returned as a JSON
	// string), not int64 — we don't actually use the value but the type has to
	// match or json.Unmarshal fails on the whole response.
	// `first: 1000` overrides the default 100-row page size on The Graph.
	const q = `
	query($ids: [String!]!) {
		liquidityPools(first: 1000, where: { id_in: $ids }) {
			id
			totalValueLockedUSD
			dailySnapshots(first: 1, orderBy: timestamp, orderDirection: desc) {
				timestamp
				dailyVolumeUSD
			}
		}
	}`
	var data struct {
		LiquidityPools []struct {
			ID                  string `json:"id"`
			TotalValueLockedUSD string `json:"totalValueLockedUSD"`
			DailySnapshots      []struct {
				Timestamp      json.Number `json:"timestamp"`
				DailyVolumeUSD string      `json:"dailyVolumeUSD"`
			} `json:"dailySnapshots"`
		} `json:"liquidityPools"`
	}
	if err := graphqlPost(ctx, url, gqlRequest{
		Query:     q,
		Variables: map[string]interface{}{"ids": addrs},
	}, &data); err != nil {
		log.Printf("[subgraph] messari metrics refresh error: %v", err)
		return
	}
	updated := 0
	for _, sp := range data.LiquidityPools {
		p, ok := registry.Get(sp.ID)
		if !ok {
			continue
		}
		tvl, _ := strconv.ParseFloat(sp.TotalValueLockedUSD, 64)
		var vol float64
		if len(sp.DailySnapshots) > 0 {
			vol, _ = strconv.ParseFloat(sp.DailySnapshots[0].DailyVolumeUSD, 64)
		}
		if tvl > SanityMaxTVLUSD {
			tvl = 0
		}
		if vol > SanityMaxTVLUSD {
			vol = 0
		}
		p.mu.Lock()
		p.TVLUSD = tvl
		p.Volume24hUSD = vol
		p.mu.Unlock()
		updated++
	}
	log.Printf("[subgraph] refreshed metrics for %d Messari pools", updated)
}

func refreshBalancerMetrics(ctx context.Context, url string, addrs []string, registry *PoolRegistry) {
	// Balancer schema: poolSnapshots indexed by timestamp; the most recent snapshot
	// holds the previous 24h swap volume in `swapVolume`. totalSwapVolume is all-time.
	const q = `
	query($ids: [String!]!) {
		pools(first: 1000, where: { address_in: $ids }) {
			address
			totalLiquidity
			snapshots(first: 1, orderBy: timestamp, orderDirection: desc) {
				timestamp
				swapVolume
			}
		}
	}`
	var data struct {
		Pools []struct {
			Address        string `json:"address"`
			TotalLiquidity string `json:"totalLiquidity"`
			Snapshots      []struct {
				Timestamp  json.Number `json:"timestamp"`
				SwapVolume string      `json:"swapVolume"`
			} `json:"snapshots"`
		} `json:"pools"`
	}
	if err := graphqlPost(ctx, url, gqlRequest{
		Query:     q,
		Variables: map[string]interface{}{"ids": addrs},
	}, &data); err != nil {
		log.Printf("[subgraph] balancer metrics refresh error: %v", err)
		return
	}
	updated := 0
	for _, sp := range data.Pools {
		p, ok := registry.Get(sp.Address)
		if !ok {
			continue
		}
		tvl, _ := strconv.ParseFloat(sp.TotalLiquidity, 64)
		var vol float64
		if len(sp.Snapshots) > 0 {
			vol, _ = strconv.ParseFloat(sp.Snapshots[0].SwapVolume, 64)
		}
		if tvl > SanityMaxTVLUSD {
			tvl = 0
		}
		if vol > SanityMaxTVLUSD {
			vol = 0
		}
		p.mu.Lock()
		p.TVLUSD = tvl
		p.Volume24hUSD = vol
		p.mu.Unlock()
		updated++
	}
	log.Printf("[subgraph] refreshed metrics for %d Balancer pools", updated)
}

func refreshCurveMetrics(ctx context.Context, url string, addrs []string, registry *PoolRegistry) {
	// Curve schema: dailyVolumes[0].volume = actual 24h volume.
	// totalVolume is all-time cumulative.
	const q = `
	query($ids: [String!]!) {
		pools(first: 1000, where: { id_in: $ids }) {
			id
			dailyVolumes(first: 1, orderBy: timestamp, orderDirection: desc) {
				timestamp
				volume
			}
		}
	}`
	var data struct {
		Pools []struct {
			ID           string `json:"id"`
			DailyVolumes []struct {
				Timestamp json.Number `json:"timestamp"`
				Volume    string      `json:"volume"`
			} `json:"dailyVolumes"`
		} `json:"pools"`
	}
	if err := graphqlPost(ctx, url, gqlRequest{
		Query:     q,
		Variables: map[string]interface{}{"ids": addrs},
	}, &data); err != nil {
		log.Printf("[subgraph] curve metrics refresh error: %v", err)
		return
	}
	updated := 0
	for _, sp := range data.Pools {
		p, ok := registry.Get(sp.ID)
		if !ok {
			continue
		}
		var vol float64
		if len(sp.DailyVolumes) > 0 {
			vol, _ = strconv.ParseFloat(sp.DailyVolumes[0].Volume, 64)
		}
		if vol > SanityMaxTVLUSD {
			vol = 0
		}
		p.mu.Lock()
		p.Volume24hUSD = vol
		p.mu.Unlock()
		updated++
	}
	log.Printf("[subgraph] refreshed metrics for %d Curve pools", updated)
}

// ── Token helper ──────────────────────────────────────────────────────────────

func resolveSubgraphToken(st sgToken, tokens *TokenRegistry) *Token {
	addr := strings.ToLower(st.ID)
	if t, ok := tokens.Get(addr); ok {
		return t
	}
	dec, _ := strconv.ParseUint(st.Decimals, 10, 8)
	t := NewToken(addr, st.Symbol, uint8(dec))
	tokens.Add(t)
	return t
}
