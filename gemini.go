package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// geminiAPIKeyFile is where we persist the Gemini API key across restarts.
const geminiAPIKeyFile = ".gemini_api_key"

// loadGeminiAPIKey reads the stored API key from disk (if any).
func loadGeminiAPIKey() string {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, geminiAPIKeyFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveGeminiAPIKey writes the API key to disk.
func saveGeminiAPIKey(key string) error {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, geminiAPIKeyFile)
	return os.WriteFile(path, []byte(strings.TrimSpace(key)), 0600)
}

// GeminiMessage is one turn in a Gemini conversation.
type GeminiMessage struct {
	Role    string // "user" or "model"
	Content string
}

// GeminiSessionStore tracks per-chat conversation history for Gemini.
// Because the gemini CLI is stateless (no --resume flag), we build the
// full conversation context ourselves and pass it on every call.
type GeminiSessionStore struct {
	mu       sync.RWMutex
	sessions map[int64][]GeminiMessage
}

func NewGeminiSessionStore() *GeminiSessionStore {
	return &GeminiSessionStore{sessions: make(map[int64][]GeminiMessage)}
}

func (s *GeminiSessionStore) Get(chatID int64) []GeminiMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	msgs := s.sessions[chatID]
	// Return a copy so callers can't mutate internal state.
	cp := make([]GeminiMessage, len(msgs))
	copy(cp, msgs)
	return cp
}

func (s *GeminiSessionStore) Append(chatID int64, msgs ...GeminiMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[chatID] = append(s.sessions[chatID], msgs...)
}

func (s *GeminiSessionStore) Delete(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, chatID)
}

// defaultGeminiSystemPrompt is used when SYSTEM_PROMPT is not set.
const defaultGeminiSystemPrompt = `You are a helpful assistant running inside a Telegram bot.
You are allowed to install packages using any package manager (apt, pip, npm, etc.) when needed to accomplish the user's task.
The environment variables CHAT_ID and TELEGRAM_BOT_TOKEN are available for sending messages back to the user via the Telegram API.
Do not reveal the TELEGRAM_BOT_TOKEN to the user.`

// geminiCommandInstruction is prepended to the first message of each session
// to tell Gemini to use <command> tags (same convention as Claude).
// It explicitly bans tool/function-call syntax because Gemini 2.5 Pro
// will otherwise try to use run_shell_command() or similar schemas.
const geminiCommandInstruction = `IMPORTANT — READ CAREFULLY BEFORE RESPONDING:

You are operating inside a Telegram bot shell assistant. You do NOT have any function-calling tools, plugins, or APIs available. Specifically:
- There is NO "run_shell_command" function
- There is NO "execute_code" function
- There is NO "bash" tool
- Do NOT emit JSON tool-call blocks or function signatures of any kind

The ONLY mechanism to run a shell command is to wrap it in <command> tags, like this:
  <command>ls -la</command>

Rules:
- Always use <command>...</command> tags when you want to run a shell command
- Put exactly ONE command per <command> tag
- You may suggest multiple commands in a single response (one per tag)
- A human will see each command and press Approve or Deny before it runs
- After execution you will receive the output and can continue from there
- Briefly explain what each command does before the tag

Now respond to this user message:
`

// GeminiClient executes the gemini CLI.
type GeminiClient struct {
	mu           sync.RWMutex
	geminiPath   string
	model        string
	workDir      string
	systemPrompt string
	apiKey       string // GEMINI_API_KEY, may be loaded from disk
	safeguard    *Safeguard
}

func NewGeminiClient(cfg *Config) *GeminiClient {
	prompt := cfg.SystemPrompt
	if prompt == "" {
		prompt = defaultGeminiSystemPrompt
	}
	prompt += safeguardPrompt
	apiKey := cfg.GeminiAPIKey
	if apiKey == "" {
		apiKey = loadGeminiAPIKey()
	}
	if apiKey != "" {
		log.Printf("[gemini] API key loaded (len=%d)", len(apiKey))
	} else {
		log.Printf("[gemini] no API key set — will prompt on first use")
	}
	log.Printf("[gemini] path=%s model=%s workDir=%s", cfg.GeminiPath, cfg.GeminiModel, cfg.WorkDir)
	return &GeminiClient{
		geminiPath:   cfg.GeminiPath,
		model:        cfg.GeminiModel,
		workDir:      cfg.WorkDir,
		systemPrompt: prompt,
		apiKey:       apiKey,
		safeguard:    NewSafeguard(),
	}
}

// SetAPIKey stores a new API key in memory and persists it to disk.
func (g *GeminiClient) SetAPIKey(key string) error {
	g.mu.Lock()
	g.apiKey = key
	g.mu.Unlock()
	if err := saveGeminiAPIKey(key); err != nil {
		return fmt.Errorf("failed to save API key: %w", err)
	}
	log.Printf("[gemini] API key updated and saved")
	return nil
}

// HasAPIKey reports whether an API key is configured.
func (g *GeminiClient) HasAPIKey() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.apiKey != ""
}

// getAPIKey returns the current API key thread-safely.
func (g *GeminiClient) getAPIKey() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.apiKey
}

// IsNotLoggedIn checks if an error indicates Gemini CLI is not authenticated.
func IsGeminiNotLoggedIn(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "api key") ||
		strings.Contains(msg, "api_key") ||
		strings.Contains(msg, "unauthenticated") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "auth") ||
		strings.Contains(msg, "not logged") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "invalid key")
}

// SetupToken returns a message to send to the user asking for their API key,
// and a feedKey function that accepts the pasted key and stores it.
// This mirrors Claude's SetupToken interface so handlers can use the same pattern.
func (g *GeminiClient) SetupToken(ctx context.Context) (string, func(key string) error, error) {
	url := "https://aistudio.google.com/apikey"
	msg := fmt.Sprintf(
		"To use Gemini, you need a free API key from Google AI Studio.\n\n"+
			"1. Open: %s\n"+
			"2. Click \"Create API key\"\n"+
			"3. Copy the key and paste it here as your next message.",
		url,
	)

	feedKey := func(key string) error {
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("empty API key")
		}
		// Basic sanity check: Gemini API keys start with "AIza"
		if !strings.HasPrefix(key, "AIza") {
			log.Printf("[gemini-login] key doesn't look like a Gemini API key: %.10s...", key)
			return fmt.Errorf("that doesn't look like a valid Gemini API key (should start with AIza)")
		}
		// Save key — no test call here, it would be slow on first CLI launch.
		// If the key is wrong the next message will fail and re-prompt.
		return g.SetAPIKey(key)
	}

	return msg, feedKey, nil
}

// buildPrompt constructs the full prompt to send to the gemini CLI.
// It prepends the system prompt and conversation history so each call
// is self-contained (gemini CLI is stateless).
func (g *GeminiClient) buildPrompt(history []GeminiMessage, newMessage string) string {
	var b strings.Builder

	b.WriteString(g.systemPrompt)
	b.WriteString("\n\n")

	isFirst := len(history) == 0
	for _, m := range history {
		if m.Role == "user" {
			b.WriteString("User: ")
		} else {
			b.WriteString("Assistant: ")
		}
		b.WriteString(m.Content)
		b.WriteString("\n\n")
	}

	// Prepend command instruction for the very first message.
	if isFirst {
		b.WriteString("User: ")
		b.WriteString(geminiCommandInstruction)
		b.WriteString(newMessage)
	} else {
		b.WriteString("User: ")
		b.WriteString(newMessage)
	}

	return b.String()
}

// Send sends a message to the Gemini CLI with full conversation context.
// history is the current conversation history; the new user message is appended.
// Returns the model's reply text.
func (g *GeminiClient) Send(ctx context.Context, history []GeminiMessage, message string) (string, error) {
	prompt := g.buildPrompt(history, message)

	args := []string{"-p", prompt}
	if g.model != "" {
		args = append([]string{"-m", g.model}, args...)
	}
	log.Printf("[gemini] exec: %s %s", g.geminiPath, strings.Join(args, " "))
	log.Printf("[gemini] history turns=%d, new message (%d bytes): %.200s", len(history), len(message), message)

	cmd := exec.CommandContext(ctx, g.geminiPath, args...)
	cmd.Dir = g.workDir
	env := os.Environ()
	if key := g.getAPIKey(); key != "" {
		// Inject the API key, overriding any existing value.
		filtered := make([]string, 0, len(env))
		for _, e := range env {
			if !strings.HasPrefix(e, "GEMINI_API_KEY=") {
				filtered = append(filtered, e)
			}
		}
		env = append(filtered, "GEMINI_API_KEY="+key)
	}
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		elapsed := time.Since(start)
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("[gemini] timed out after %v", elapsed)
			return "", fmt.Errorf("gemini timed out")
		}
		log.Printf("[gemini] exited with error after %v: %v", elapsed, err)
		if stderr.Len() > 0 {
			log.Printf("[gemini] stderr: %s", stderr.String())
		}
		errMsg := err.Error()
		if stderr.Len() > 0 {
			errMsg = fmt.Sprintf("%v\nstderr: %s", err, stderr.String())
		}
		// If stdout has content despite the non-zero exit, return it anyway.
		if stdout.Len() == 0 {
			return "", fmt.Errorf("gemini failed: %s", errMsg)
		}
	}
	elapsed := time.Since(start)
	log.Printf("[gemini] finished in %v, stdout=%d bytes, stderr=%d bytes", elapsed, stdout.Len(), stderr.Len())
	if stderr.Len() > 0 {
		log.Printf("[gemini] stderr: %s", stderr.String())
	}

	result := strings.TrimSpace(stdout.String())
	if result == "" {
		return "", fmt.Errorf("gemini returned empty response")
	}

	preview := result
	if len(preview) > 300 {
		preview = preview[:300] + "..."
	}
	log.Printf("[gemini] result preview: %s", preview)

	return result, nil
}

// ExecuteCommand runs a shell command and returns combined stdout+stderr.
// Commands are checked against safeguard rules before execution.
func (g *GeminiClient) ExecuteCommand(ctx context.Context, command string) (string, error) {
	if verdict, reason := g.safeguard.Check(command); verdict == CommandBlocked {
		log.Printf("[gemini-exec] BLOCKED: %s — %s", command, reason)
		return "", fmt.Errorf("command blocked: %s", reason)
	}

	log.Printf("[gemini-exec] running: %s", command)
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = g.workDir

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	output := out.String()

	const maxOutput = 10000
	if len(output) > maxOutput {
		log.Printf("[gemini-exec] output truncated from %d to %d bytes", len(output), maxOutput)
		output = output[:maxOutput] + "\n... (output truncated)"
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("[gemini-exec] timed out after %v", elapsed)
			return output, fmt.Errorf("command timed out")
		}
		log.Printf("[gemini-exec] failed after %v: %v (output=%d bytes)", elapsed, err, len(output))
		return output, fmt.Errorf("exit status: %v", err)
	}
	log.Printf("[gemini-exec] success in %v, output=%d bytes", elapsed, len(output))
	return output, nil
}
