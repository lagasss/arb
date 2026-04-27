package internal

import (
	"context"
	"crypto/tls"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// DialHTTP2 dials an HTTPS JSON-RPC endpoint using an HTTP/2-capable
// http.Client so parallel requests multiplex over a single TLS connection.
// Falls back to HTTP/1.1 automatically if the server doesn't negotiate h2 via
// ALPN (standard HTTP/2 behaviour).
//
// Uses the stdlib http.Transport with ForceAttemptHTTP2=true — no external
// dep on golang.org/x/net/http2 required. The stdlib has built-in HTTP/2
// support since Go 1.6 and auto-upgrades the connection when ALPN negotiates
// h2 with the server.
//
// Use for simulation_rpc, tick_data_rpc, and any other HTTPS endpoint that
// sees concurrent batch multicalls. DO NOT use for WSS endpoints — WebSockets
// tunnel over HTTP/1.1 by design and go-ethereum's websocket dialer is a
// different code path that ignores the HTTP client.
func DialHTTP2(url string) (*ethclient.Client, error) {
	base := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		MaxConnsPerHost:       16,
		IdleConnTimeout:       5 * time.Minute,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	httpClient := &http.Client{
		Transport: base,
		Timeout:   30 * time.Second,
	}
	client, err := rpc.DialOptions(context.Background(), url, rpc.WithHTTPClient(httpClient))
	if err != nil {
		return nil, err
	}
	return ethclient.NewClient(client), nil
}
