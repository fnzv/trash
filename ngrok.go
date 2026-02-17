package main

import (
	"log"
	"os/exec"
)

func SetupNgrok(cfg *Config) error {
	if cfg.NgrokToken == "" {
		log.Println("Ngrok token not provided, skipping ngrok authtoken setup.")
		return nil
	}

	log.Println("Setting ngrok authtoken via CLI...")

	cmd := exec.Command("ngrok", "config", "add-authtoken", cfg.NgrokToken)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Failed to set ngrok authtoken: %v\nOutput: %s", err, string(output))
		return err
	}

	log.Println("Ngrok authtoken set successfully.")
	return nil
}
