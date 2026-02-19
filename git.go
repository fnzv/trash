package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func SetupGit(cfg *Config) error {
	if cfg.GitUserName != "" {
		if err := exec.Command("git", "config", "--global", "user.name", cfg.GitUserName).Run(); err != nil {
			return fmt.Errorf("set git user.name: %w", err)
		}
	}

	if cfg.GitUserEmail != "" {
		if err := exec.Command("git", "config", "--global", "user.email", cfg.GitUserEmail).Run(); err != nil {
			return fmt.Errorf("set git user.email: %w", err)
		}
	}

	if cfg.GitSSHKey != "" {
		keyData, err := base64.StdEncoding.DecodeString(cfg.GitSSHKey)
		if err != nil {
			keyData = []byte(cfg.GitSSHKey)
		}
		home, _ := os.UserHomeDir()
		sshDir := filepath.Join(home, ".ssh")
		if err := os.MkdirAll(sshDir, 0700); err != nil {
			return fmt.Errorf("create .ssh dir: %w", err)
		}
		keyPath := filepath.Join(sshDir, "id_ed25519")
		if err := os.WriteFile(keyPath, keyData, 0600); err != nil {
			return fmt.Errorf("write SSH key: %w", err)
		}
		configPath := filepath.Join(sshDir, "config")
		sshConfig := "Host *\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n"
		if err := os.WriteFile(configPath, []byte(sshConfig), 0600); err != nil {
			return fmt.Errorf("write SSH config: %w", err)
		}
	}

	if cfg.GitlabToken != "" {
		os.Setenv("GITLAB_TOKEN", cfg.GitlabToken)
	}

	return nil
}
