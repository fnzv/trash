package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	if err := SetupGit(cfg); err != nil {
		log.Printf("WARN: git setup failed: %v", err)
	}

	if err := SetupNgrok(cfg); err != nil {
		log.Printf("WARN: ngrok setup failed: %v", err)
	}

	bot, err := NewBot(cfg)
	if err != nil {
		log.Fatalf("bot init error: %v", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go bot.Run()

	log.Println("Bot is running. Press Ctrl+C to stop.")
	<-stop
	log.Println("Shutting down...")
}
