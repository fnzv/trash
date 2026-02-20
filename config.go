package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	TelegramToken   string
	AllowedChatIDs  map[int64]bool
	WorkDir         string
	ClaudePath      string
	GeminiPath      string
	GeminiAPIKey    string
	GeminiModel     string
	DefaultProvider string
	CommandTimeout  time.Duration
	AllowedTools    []string
	SkipPermissions bool
	SystemPrompt    string
	MaxToolRounds   int
	WhisperCmd      string
	GitSSHKey       string
	GitlabToken     string
	GitUserName     string
	GitUserEmail    string
	NgrokToken      string
}

func LoadConfig() (*Config, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	allowedRaw := os.Getenv("ALLOWED_CHAT_IDS")
	if allowedRaw == "" {
		return nil, fmt.Errorf("ALLOWED_CHAT_IDS is required")
	}

	allowed := make(map[int64]bool)
	for _, s := range strings.Split(allowedRaw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid chat ID %q: %v", s, err)
		}
		allowed[id] = true
	}

	workDir := os.Getenv("WORK_DIR")
	if workDir == "" {
		workDir = "."
	}

	claudePath := os.Getenv("CLAUDE_PATH")
	if claudePath == "" {
		claudePath = "claude"
	}

	geminiPath := os.Getenv("GEMINI_PATH")
	if geminiPath == "" {
		geminiPath = "gemini"
	}

	geminiModel := os.Getenv("GEMINI_MODEL")
	if geminiModel == "" {
		geminiModel = "gemini-2.5-flash"
	}

	defaultProvider := os.Getenv("DEFAULT_PROVIDER")
	if defaultProvider == "" {
		defaultProvider = "claude"
	}

	timeout := 5 * time.Minute
	if t := os.Getenv("COMMAND_TIMEOUT"); t != "" {
		var err error
		timeout, err = time.ParseDuration(t)
		if err != nil {
			return nil, fmt.Errorf("invalid COMMAND_TIMEOUT %q: %v", t, err)
		}
	}

	var allowedTools []string
	if toolsRaw := os.Getenv("ALLOWED_TOOLS"); toolsRaw != "" {
		for _, t := range strings.Split(toolsRaw, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				allowedTools = append(allowedTools, t)
			}
		}
	}

	skipPerms := os.Getenv("SKIP_PERMISSIONS") == "true"
	systemPrompt := os.Getenv("SYSTEM_PROMPT")

	whisperCmd := os.Getenv("WHISPER_CMD")
	if whisperCmd == "" {
		whisperCmd = "whisper"
	}

	maxRounds := 20
	if r := os.Getenv("MAX_TOOL_ROUNDS"); r != "" {
		if v, err := strconv.Atoi(r); err == nil && v > 0 {
			maxRounds = v
		}
	}

	return &Config{
		TelegramToken:   token,
		AllowedChatIDs:  allowed,
		WorkDir:         workDir,
		ClaudePath:      claudePath,
		GeminiPath:      geminiPath,
		GeminiAPIKey:    os.Getenv("GEMINI_API_KEY"),
		GeminiModel:     geminiModel,
		DefaultProvider: defaultProvider,
		CommandTimeout:  timeout,
		AllowedTools:    allowedTools,
		SkipPermissions: skipPerms,
		SystemPrompt:    systemPrompt,
		MaxToolRounds:   maxRounds,
		WhisperCmd:      whisperCmd,
		GitSSHKey:       os.Getenv("GIT_SSH_KEY"),
		GitlabToken:     os.Getenv("GITLAB_TOKEN"),
		GitUserName:     os.Getenv("GIT_USER_NAME"),
		GitUserEmail:    os.Getenv("GIT_USER_EMAIL"),
		NgrokToken:      os.Getenv("NGROK_AUTHTOKEN"),
	}, nil
}
