package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"arb-bot/internal"

	"gopkg.in/yaml.v3"
)

func main() {
	// Load config
	cfgPath := "config.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	// Change to config file's directory so relative paths (arb.db, etc.) resolve correctly.
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

	// Load private key from environment only -- never from config file
	privKey := os.Getenv("ARB_BOT_PRIVKEY")

	bot, err := internal.NewBot(&cfg, privKey)
	if err != nil {
		log.Fatalf("init bot: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("received shutdown signal")
		cancel()
	}()

	bot.Run(ctx)
}
