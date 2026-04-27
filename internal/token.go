package internal

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

var erc20ABI abi.ABI

func init() {
	var err error
	erc20ABI, err = abi.JSON(strings.NewReader(`[
		{"name":"symbol","type":"function","inputs":[],"outputs":[{"name":"","type":"string"}],"stateMutability":"view"},
		{"name":"decimals","type":"function","inputs":[],"outputs":[{"name":"","type":"uint8"}],"stateMutability":"view"}
	]`))
	if err != nil {
		panic(fmt.Sprintf("erc20ABI: %v", err))
	}
}

// FetchTokenMeta calls decimals() and symbol() on-chain and returns a Token.
func FetchTokenMeta(ctx context.Context, client *ethclient.Client, address string) *Token {
	addr := common.HexToAddress(address)

	// decimals()
	decimals := uint8(18)
	decimalsData, _ := erc20ABI.Pack("decimals")
	res, err := client.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: decimalsData}, nil)
	if err == nil && len(res) >= 1 {
		decimals = res[len(res)-1]
	}

	// symbol()
	symbol := "UNK"
	symbolData, _ := erc20ABI.Pack("symbol")
	res, err = client.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: symbolData}, nil)
	if err == nil {
		vals, unpackErr := erc20ABI.Unpack("symbol", res)
		if unpackErr == nil && len(vals) > 0 {
			if s, ok := vals[0].(string); ok {
				symbol = s
			}
		} else {
			// try bytes32 fallback
			if len(res) >= 32 {
				b := [32]byte{}
				copy(b[:], res[:32])
				symbol = strings.TrimRight(string(b[:]), "\x00")
			}
		}
	}

	return NewToken(strings.ToLower(address), symbol, decimals)
}

type Token struct {
	Address  string  // lowercase hex
	Symbol   string
	Decimals uint8
	Scalar   float64 // cached 1/10^Decimals
}

func NewToken(address, symbol string, decimals uint8) *Token {
	return &Token{
		Address:  strings.ToLower(address),
		Symbol:   symbol,
		Decimals: decimals,
		Scalar:   1.0 / math.Pow(10, float64(decimals)),
	}
}

// NativeETHAddress is the canonical address for native ETH on Uniswap V4
// PoolKeys (currency0/currency1 == address(0)). The registry aliases lookups
// of this address to WETH metadata so V4 ETH pools route through the cycle
// graph identically to WETH-paired pools, while the V4 executor still uses
// 0x0000... when constructing the PoolKey for the on-chain swap call.
const NativeETHAddress = "0x0000000000000000000000000000000000000000"

// WETHArbitrumAddress is the canonical WETH9 address on Arbitrum One. Used as
// the alias target for native ETH lookups in TokenRegistry.Get.
const WETHArbitrumAddress = "0x82af49447d8a07e3bd95bd0d56f35241523fbab1"

type TokenRegistry struct {
	mu     sync.RWMutex
	tokens map[string]*Token // address → token
}

func NewTokenRegistry() *TokenRegistry {
	return &TokenRegistry{tokens: make(map[string]*Token)}
}

func (r *TokenRegistry) Add(t *Token) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens[strings.ToLower(t.Address)] = t
}

func (r *TokenRegistry) Get(address string) (*Token, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tokens[strings.ToLower(address)]
	return t, ok
}

func (r *TokenRegistry) All() []*Token {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Token, 0, len(r.tokens))
	for _, t := range r.tokens {
		out = append(out, t)
	}
	return out
}

func (r *TokenRegistry) IsWhitelisted(address string) bool {
	_, ok := r.Get(address)
	return ok
}
