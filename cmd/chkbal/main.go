//go:build ignore

package main

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

func main() {
	client, _ := ethclient.Dial("wss://arbitrum-mainnet.core.chainstack.com/3f4c4d6546df8aabd1d7a56baf76a18b")
	defer client.Close()

	curvePool := common.HexToAddress("0x7f90122bf0700f9e7e1f688fe926940e8839f353")

	// Call get_dy to understand pool indices and see what tokens are in it
	getDyABI, _ := abi.JSON(strings.NewReader(`[
		{"name":"get_dy","type":"function","inputs":[
			{"name":"i","type":"int128"},
			{"name":"j","type":"int128"},
			{"name":"dx","type":"uint256"}
		],"outputs":[{"name":"","type":"uint256"}]},
		{"name":"coins","type":"function","inputs":[{"name":"i","type":"uint256"}],"outputs":[{"name":"","type":"address"}]}
	]`))

	// Check coins(0) and coins(1)
	for i := 0; i <= 2; i++ {
		data, _ := getDyABI.Pack("coins", big.NewInt(int64(i)))
		result, err := client.CallContract(context.Background(), ethereum.CallMsg{To: &curvePool, Data: data}, nil)
		if err != nil { fmt.Printf("coins(%d): error %v\n", i, err); break }
		var addr common.Address
		if len(result) >= 32 { addr = common.HexToAddress(common.BytesToHash(result[12:32]).Hex()) }
		fmt.Printf("coins(%d) = %s\n", i, addr.Hex())
	}

	// Try get_dy(0,1, 1e18) to see if exchange(int128) returns a value
	data, _ := getDyABI.Pack("get_dy", big.NewInt(0), big.NewInt(1), new(big.Int).Mul(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))
	result, err := client.CallContract(context.Background(), ethereum.CallMsg{To: &curvePool, Data: data}, nil)
	if err != nil {
		fmt.Println("get_dy error:", err)
	} else {
		val := new(big.Int).SetBytes(result)
		fmt.Printf("get_dy(0,1,1e18) = %s\n", val.String())
	}

	// Now simulate exchange with no return value expected — use raw call
	exchangeNoReturn, _ := abi.JSON(strings.NewReader(`[{"name":"exchange","type":"function","inputs":[
		{"name":"i","type":"int128"},{"name":"j","type":"int128"},
		{"name":"dx","type":"uint256"},{"name":"min_dy","type":"uint256"}
	],"outputs":[]}]`))
	data2, _ := exchangeNoReturn.Pack("exchange", big.NewInt(0), big.NewInt(1), big.NewInt(1000000000000000000), big.NewInt(1))
	caller := common.HexToAddress("0x24B32229baE5e5Ad04ffD4b0D7CFFf7c23770674")
	result2, err2 := client.CallContract(context.Background(), ethereum.CallMsg{From: caller, To: &curvePool, Data: data2}, nil)
	fmt.Printf("exchange(no-return) raw call: returndata_len=%d err=%v\n", len(result2), err2)
}
