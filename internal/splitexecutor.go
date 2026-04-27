package internal

// SplitExecutor submits SplitArb.sol transactions to the chain.
// It mirrors the pattern of Executor but encodes TradeParams for the new contract.

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// SplitArb.sol ABI fragments needed for encoding.
const splitArbABI = `[
  {
    "name": "execute",
    "type": "function",
    "inputs": [
      {
        "name": "p",
        "type": "tuple",
        "components": [
          {"name": "borrowToken",   "type": "address"},
          {"name": "borrowAmount",  "type": "uint256"},
          {"name": "tradeToken",    "type": "address"},
          {"name": "buyPool",       "type": "address"},
          {"name": "buyIsV3",       "type": "bool"},
          {"name": "buyZeroForOne", "type": "bool"},
          {"name": "buyMinOut",     "type": "uint256"},
          {
            "name": "sellHops",
            "type": "tuple[]",
            "components": [
              {"name": "pool",       "type": "address"},
              {"name": "isV3",       "type": "bool"},
              {"name": "zeroForOne", "type": "bool"},
              {"name": "amountIn",   "type": "uint256"}
            ]
          },
          {"name": "minProfitWei", "type": "uint256"}
        ]
      }
    ],
    "outputs": []
  }
]`

// SplitExecutor handles transaction submission for the SplitArb contract.
type SplitExecutor struct {
	client   *ethclient.Client
	key      *ecdsa.PrivateKey
	from     common.Address
	contract common.Address
	chainID  *big.Int
	parsedABI abi.ABI
	seqClient *ethclient.Client // optional fast sequencer
}

func NewSplitExecutor(client *ethclient.Client, privKey string, contractAddr string, chainID *big.Int, seqRPC string) (*SplitExecutor, error) {
	keyBytes, err := hex.DecodeString(strings.TrimPrefix(privKey, "0x"))
	if err != nil {
		return nil, fmt.Errorf("decode privkey: %w", err)
	}
	key, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse privkey: %w", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)

	parsed, err := abi.JSON(strings.NewReader(splitArbABI))
	if err != nil {
		return nil, fmt.Errorf("parse splitarb abi: %w", err)
	}

	ex := &SplitExecutor{
		client:    client,
		key:       key,
		from:      from,
		contract:  common.HexToAddress(contractAddr),
		chainID:   chainID,
		parsedABI: parsed,
	}

	if seqRPC != "" {
		sc, err := ethclient.Dial(seqRPC)
		if err != nil {
			log.Printf("[splitexec] sequencer dial failed (%v), using main RPC", err)
		} else {
			ex.seqClient = sc
			log.Printf("[splitexec] submit client connected to sequencer: %s", seqRPC)
		}
	}
	return ex, nil
}

// Submit encodes and submits a SplitArbOpp transaction. Returns the tx hash.
func (e *SplitExecutor) Submit(ctx context.Context, opp *SplitArbOpp, slippageBps int64) (string, error) {
	// Encode the call
	p := opp.ToOnChainParams(slippageBps, big.NewInt(1))

	// Build the tuple for ABI encoding
	type sellHopTuple struct {
		Pool       common.Address `abi:"pool"`
		IsV3       bool           `abi:"isV3"`
		ZeroForOne bool           `abi:"zeroForOne"`
		AmountIn   *big.Int       `abi:"amountIn"`
	}
	hops := make([]sellHopTuple, len(p.SellHops))
	for i, h := range p.SellHops {
		hops[i] = sellHopTuple{h.Pool, h.IsV3, h.ZeroForOne, h.AmountIn}
	}

	type tradeTuple struct {
		BorrowToken   common.Address  `abi:"borrowToken"`
		BorrowAmount  *big.Int        `abi:"borrowAmount"`
		TradeToken    common.Address  `abi:"tradeToken"`
		BuyPool       common.Address  `abi:"buyPool"`
		BuyIsV3       bool            `abi:"buyIsV3"`
		BuyZeroForOne bool            `abi:"buyZeroForOne"`
		BuyMinOut     *big.Int        `abi:"buyMinOut"`
		SellHops      []sellHopTuple  `abi:"sellHops"`
		MinProfitWei  *big.Int        `abi:"minProfitWei"`
	}

	data, err := e.parsedABI.Pack("execute", tradeTuple{
		BorrowToken:   p.BorrowToken,
		BorrowAmount:  p.BorrowAmount,
		TradeToken:    p.TradeToken,
		BuyPool:       p.BuyPool,
		BuyIsV3:       p.BuyIsV3,
		BuyZeroForOne: p.BuyZeroForOne,
		BuyMinOut:     p.BuyMinOut,
		SellHops:      hops,
		MinProfitWei:  p.MinProfitWei,
	})
	if err != nil {
		return "", fmt.Errorf("abi pack: %w", err)
	}

	// Simulate first
	callMsg := ethereum.CallMsg{
		From: e.from,
		To:   &e.contract,
		Data: data,
	}
	if _, err := e.client.CallContract(ctx, callMsg, nil); err != nil {
		return "", fmt.Errorf("simulation: %w", err)
	}

	// Get nonce + gas price
	nonce, err := e.client.PendingNonceAt(ctx, e.from)
	if err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	gasPrice, err := e.client.SuggestGasPrice(ctx)
	if err != nil {
		return "", fmt.Errorf("gas price: %w", err)
	}
	gasPrice = new(big.Int).Mul(gasPrice, big.NewInt(110))
	gasPrice.Div(gasPrice, big.NewInt(100)) // +10% tip

	tx := types.NewTransaction(nonce, e.contract, big.NewInt(0), 800_000, gasPrice, data)
	signed, err := types.SignTx(tx, types.NewLondonSigner(e.chainID), e.key)
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}

	submitClient := e.client
	if e.seqClient != nil {
		submitClient = e.seqClient
	}
	if err := submitClient.SendTransaction(ctx, signed); err != nil {
		return "", fmt.Errorf("send: %w", err)
	}
	return signed.Hash().Hex(), nil
}
