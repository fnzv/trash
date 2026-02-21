package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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

// geminiCommandInstruction is prepended to the very first user message.
const geminiCommandInstruction = `IMPORTANT — READ CAREFULLY:

You are a shell assistant running inside a Telegram bot. You have FULL ability to run shell commands.
You have NO built-in tools, plugins, or function-calling APIs. The ONLY mechanism to execute a command is:

  <command>your shell command here</command>

RULES:
1. Always use <command>...</command> tags on their own line when you want to run a shell command.
2. Send ONLY ONE <command> per response — wait for the output before sending the next command.
3. Do NOT write "run_shell_command", JSON tool-calls, or any other syntax. Only <command> tags.
4. Working directory persists between commands (cd works).
5. If a command starts a long-running process (server, etc.), it will be backgrounded automatically.
6. Explain briefly what the command does, then put the tag on its own line.

Now respond to this user message:
`

// --- Gemini REST API types ---

type geminiAPIRequest struct {
	SystemInstruction *geminiContent  `json:"system_instruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	GenerationConfig  *geminiGenCfg   `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenCfg struct {
	Temperature float64 `json:"temperature"`
}

type geminiAPIResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// GeminiClient calls the Gemini REST API directly.
type GeminiClient struct {
	mu           sync.RWMutex
	model        string
	workDir      string
	cwd          string // tracks the current working directory across commands
	systemPrompt string
	apiKey       string
	safeguard    *Safeguard
	httpClient   *http.Client
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
	model := cfg.GeminiModel
	if model == "" {
		model = "gemini-2.5-flash"
	}
	log.Printf("[gemini] model=%s workDir=%s (using REST API)", model, cfg.WorkDir)
	return &GeminiClient{
		model:        model,
		workDir:      cfg.WorkDir,
		cwd:          cfg.WorkDir,
		systemPrompt: prompt,
		apiKey:       apiKey,
		safeguard:    NewSafeguard(),
		httpClient:   &http.Client{Timeout: 120 * time.Second},
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

// SetModel changes the active Gemini model at runtime.
func (g *GeminiClient) SetModel(model string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.model = model
	log.Printf("[gemini] model changed to %s", model)
}

// GetModel returns the currently active model.
func (g *GeminiClient) GetModel() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.model
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

// IsGeminiNotLoggedIn checks if an error indicates missing/invalid API key.
func IsGeminiNotLoggedIn(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "api key") ||
		strings.Contains(msg, "api_key") ||
		strings.Contains(msg, "unauthenticated") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "not logged") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "invalid key")
}

// SetupToken returns a message asking for the API key and a callback to store it.
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
		if !strings.HasPrefix(key, "AIza") {
			log.Printf("[gemini-login] key doesn't look like a Gemini API key: %.10s...", key)
			return fmt.Errorf("that doesn't look like a valid Gemini API key (should start with AIza)")
		}
		return g.SetAPIKey(key)
	}

	return msg, feedKey, nil
}

// Send sends a message to the Gemini REST API with full conversation context.
func (g *GeminiClient) Send(ctx context.Context, history []GeminiMessage, message string) (string, error) {
	apiKey := g.getAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("api key not set")
	}

	// Build contents from history.
	var contents []geminiContent
	isFirst := len(history) == 0
	for _, m := range history {
		role := m.Role
		if role == "model" {
			role = "model"
		}
		contents = append(contents, geminiContent{
			Role:  role,
			Parts: []geminiPart{{Text: m.Content}},
		})
	}

	// Prepend command instruction only on the very first message.
	userText := message
	if isFirst {
		userText = geminiCommandInstruction + message
	}
	contents = append(contents, geminiContent{
		Role:  "user",
		Parts: []geminiPart{{Text: userText}},
	})

	reqBody := geminiAPIRequest{
		SystemInstruction: &geminiContent{
			Parts: []geminiPart{{Text: g.systemPrompt}},
		},
		Contents: contents,
		GenerationConfig: &geminiGenCfg{
			Temperature: 1.0,
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	endpoint := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		g.model, apiKey,
	)

	log.Printf("[gemini] REST API call: model=%s history_turns=%d new_message_len=%d", g.model, len(history), len(message))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	log.Printf("[gemini] API response in %v: status=%d body_len=%d", elapsed, resp.StatusCode, len(respBody))

	var apiResp geminiAPIResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return "", fmt.Errorf("unmarshal response: %w\nraw: %.500s", err, respBody)
	}

	if apiResp.Error != nil {
		msg := apiResp.Error.Message
		log.Printf("[gemini] API error %d %s: %s", apiResp.Error.Code, apiResp.Error.Status, msg)
		return "", fmt.Errorf("gemini API error (%d %s): %s", apiResp.Error.Code, apiResp.Error.Status, msg)
	}

	if len(apiResp.Candidates) == 0 {
		return "", fmt.Errorf("gemini returned no candidates (raw: %.300s)", respBody)
	}

	candidate := apiResp.Candidates[0]
	var parts []string
	for _, p := range candidate.Content.Parts {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	result := strings.TrimSpace(strings.Join(parts, ""))
	if result == "" {
		return "", fmt.Errorf("gemini returned empty response (finishReason=%s)", candidate.FinishReason)
	}

	preview := result
	if len(preview) > 300 {
		preview = preview[:300] + "..."
	}
	log.Printf("[gemini] result preview: %s", preview)
	return result, nil
}

// getCwd returns the current tracked working directory thread-safely.
func (g *GeminiClient) getCwd() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.cwd != "" {
		return g.cwd
	}
	return g.workDir
}

// setCwd updates the tracked working directory thread-safely.
func (g *GeminiClient) setCwd(dir string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cwd = dir
}

// bgTimeout is how long we wait for a command before backgrounding it.
const bgTimeout = 15 * time.Second

// ExecuteCommand runs a shell command, returning its output.
// If the command doesn't exit within bgTimeout it is detached into the
// background and the caller gets whatever output was produced so far.
// The working directory persists across calls via the cwd tracker.
func (g *GeminiClient) ExecuteCommand(ctx context.Context, command string) (string, error) {
	if verdict, reason := g.safeguard.Check(command); verdict == CommandBlocked {
		log.Printf("[gemini-exec] BLOCKED: %s — %s", command, reason)
		return "", fmt.Errorf("command blocked: %s", reason)
	}

	cwd := g.getCwd()
	log.Printf("[gemini-exec] cwd=%s running: %s", cwd, command)

	// Wrap command: cd into tracked cwd, run the command, then echo the final pwd
	// so we can track directory changes.
	wrapped := fmt.Sprintf("cd %s && %s; echo; echo __CWD__:$(pwd)", shellQuote(cwd), command)

	cmd := exec.Command("sh", "-c", wrapped)
	cmd.Dir = g.workDir

	// Use a pipe so we can read output incrementally.
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start command: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// We pick the shorter of bgTimeout and whatever deadline ctx has left.
	waitCtx, waitCancel := context.WithTimeout(ctx, bgTimeout)
	defer waitCancel()

	select {
	case err := <-done:
		// Process exited normally (or with error) within bgTimeout.
		elapsed := time.Since(time.Now())
		raw := out.String()
		output, newCwd := extractCwd(raw, cwd)
		if newCwd != cwd {
			log.Printf("[gemini-exec] cwd changed: %s → %s", cwd, newCwd)
			g.setCwd(newCwd)
		}
		output = truncateOutput(output)
		if err != nil {
			log.Printf("[gemini-exec] failed (%v): %v", elapsed, err)
			return output, fmt.Errorf("exit status: %v", err)
		}
		log.Printf("[gemini-exec] success, output=%d bytes", len(output))
		return output, nil

	case <-waitCtx.Done():
		if ctx.Err() != nil {
			// Parent context cancelled — kill the process.
			cmd.Process.Kill()
			return truncateOutput(out.String()), fmt.Errorf("command timed out")
		}
		// bgTimeout fired but ctx is still alive — process is a long-runner.
		// Leave it running, return what we have so far (without killing).
		pid := cmd.Process.Pid
		log.Printf("[gemini-exec] command still running after %v — backgrounded (PID %d): %s", bgTimeout, pid, command)
		output := truncateOutput(out.String())
		if output == "" {
			output = "(no output yet)"
		}
		return fmt.Sprintf("%s\n[Process running in background, PID: %d]", output, pid), nil
	}
}

// extractCwd parses the __CWD__:<path> trailer from raw command output,
// returning the clean output and the new working directory.
func extractCwd(raw, currentCwd string) (output, newCwd string) {
	newCwd = currentCwd
	output = raw
	if idx := strings.LastIndex(raw, "\n__CWD__:"); idx >= 0 {
		trailer := strings.TrimSpace(raw[idx+len("\n__CWD__:"):])
		if trailer != "" {
			newCwd = trailer
		}
		output = strings.TrimRight(raw[:idx], "\n")
	}
	return
}

// truncateOutput caps output at 10 000 bytes.
func truncateOutput(s string) string {
	const maxOutput = 10000
	if len(s) > maxOutput {
		return s[:maxOutput] + "\n... (output truncated)"
	}
	return s
}

// shellQuote wraps a path in single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
