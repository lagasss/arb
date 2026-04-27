package main

import (
	"fmt"
	"arb-bot/internal"
)

func main() {
	db, err := internal.OpenDB("/home/arbitrator/go/arb-bot/arb.db")
	if err != nil { fmt.Println("open error:", err); return }
	defer db.Close()
	addrs, err := db.JunkTokenAddresses()
	fmt.Println("err:", err)
	fmt.Println("addrs:", addrs)
}
