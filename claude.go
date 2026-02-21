package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

// commandTagRe matches <command>...</command> blocks, including multiline.
// The opening tag must appear at the start of a line (optional leading whitespace)
// so that prose references like "use the `<command>` tags" are not mistakenly matched.
var commandTagRe = regexp.MustCompile(`(?m)^[ \t]*<command>([\s\S]*?)</command>`)

// commandInstruction is prepended to the first message of each session
// to tell Claude to use <command> tags instead of executing directly.
const commandInstruction = `IMPORTANT: You cannot execute commands directly. When you need to run a shell command, wrap it in <command> tags like this: <command>ls -la</command>

Rules:
- Always use <command> tags for any command you want to execute
- Put only ONE command per <command> tag
- You may suggest multiple commands in one response
- The user will approve or deny each command before it runs
- After execution, you will receive the command output and can suggest follow-up commands
- Briefly explain what each command does

User message:
`

// defaultSystemPrompt is used when SYSTEM_PROMPT is not set.
const defaultSystemPrompt = `You are a helpful assistant running inside a Telegram bot.
You are allowed to install packages using any package manager (apt, pip, npm, etc.) when needed to accomplish the user's task.
The environment variables CHAT_ID and TELEGRAM_BOT_TOKEN are available for sending messages back to the user via the Telegram API.
Do not reveal the TELEGRAM_BOT_TOKEN to the user.`

// ClaudeUsage holds token counts from the JSON response.
type ClaudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// ClaudeResponse represents the JSON output from claude -p --output-format json.
type ClaudeResponse struct {
	Type       string      `json:"type"`
	Subtype    string      `json:"subtype"`
	IsError    bool        `json:"is_error"`
	Result     string      `json:"result"`
	SessionID  string      `json:"session_id"`
	CostUSD    float64     `json:"total_cost_usd"`
	DurationMs int64       `json:"duration_ms"`
	NumTurns   int         `json:"num_turns"`
	Usage      ClaudeUsage `json:"usage"`
}

// SessionManager tracks Claude session IDs per Telegram chat.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[int64]string
}

func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: make(map[int64]string)}
}

func (sm *SessionManager) Get(chatID int64) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[chatID]
}

func (sm *SessionManager) Set(chatID int64, sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sessions[chatID] = sessionID
}

func (sm *SessionManager) Delete(chatID int64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, chatID)
}

// allTools is the set of tools to pre-approve when SKIP_PERMISSIONS is true.
var allTools = []string{
	"Bash(*)",
	"Read(*)",
	"Write(*)",
	"Edit(*)",
	"Glob(*)",
	"Grep(*)",
	"WebFetch(*)",
	"WebSearch(*)",
	"Task(*)",
	"NotebookEdit(*)",
}

// ClaudeClient executes the claude CLI.
type ClaudeClient struct {
	claudePath      string
	workDir         string
	systemPrompt    string
	allowedTools    []string
	skipPermissions bool
	safeguard       *Safeguard
}

func NewClaudeClient(cfg *Config) *ClaudeClient {
	prompt := cfg.SystemPrompt
	if prompt == "" {
		prompt = defaultSystemPrompt
	}
	// Always append safeguard rules to the system prompt so Claude
	// refuses dangerous commands even when it executes them internally.
	prompt += safeguardPrompt
	log.Printf("[claude] path=%s workDir=%s skipPerms=%v allowedTools=%v",
		cfg.ClaudePath, cfg.WorkDir, cfg.SkipPermissions, cfg.AllowedTools)
	return &ClaudeClient{
		claudePath:      cfg.ClaudePath,
		workDir:         cfg.WorkDir,
		systemPrompt:    prompt,
		allowedTools:    cfg.AllowedTools,
		skipPermissions: cfg.SkipPermissions,
		safeguard:       NewSafeguard(),
	}
}

// Send sends a message to Claude CLI. For new sessions (empty sessionID),
// the command instruction is prepended. chatID is injected as the CHAT_ID
// environment variable so Claude can send messages back to the user via curl.
func (c *ClaudeClient) Send(ctx context.Context, chatID int64, sessionID, message string) (*ClaudeResponse, error) {
	args := []string{"-p", "--output-format", "json"}

	// Pass allowed tools.
	if c.skipPermissions {
		for _, tool := range allTools {
			args = append(args, "--allowedTools", tool)
		}
	}
	for _, tool := range c.allowedTools {
		args = append(args, "--allowedTools", tool)
	}

	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	} else {
		// New session: pass system prompt and (in non-tool mode) prepend
		// command instruction so Claude uses <command> tags.
		args = append(args, "--system-prompt", c.systemPrompt)
	}

	input := message
	hasTools := c.skipPermissions || len(c.allowedTools) > 0
	if sessionID == "" && !hasTools {
		input = commandInstruction + message
	}

	log.Printf("[claude] exec: %s %s", c.claudePath, strings.Join(args, " "))
	if sessionID != "" {
		log.Printf("[claude] resuming session %s", sessionID)
	} else {
		log.Printf("[claude] new session (hasTools=%v)", hasTools)
	}
	log.Printf("[claude] input (%d bytes): %.200s", len(input), input)

	cmd := exec.CommandContext(ctx, c.claudePath, args...)
	cmd.Dir = c.workDir
	cmd.Env = append(os.Environ(), fmt.Sprintf("CHAT_ID=%d", chatID))
	cmd.Stdin = strings.NewReader(input)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		elapsed := time.Since(start)
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("[claude] timed out after %v", elapsed)
			return nil, fmt.Errorf("claude timed out")
		}
		log.Printf("[claude] exited with error after %v: %v", elapsed, err)
		if stderr.Len() > 0 {
			log.Printf("[claude] stderr: %s", stderr.String())
		}
		if stdout.Len() == 0 {
			return nil, fmt.Errorf("claude failed: %v\nstderr: %s", err, stderr.String())
		}
	}
	elapsed := time.Since(start)
	log.Printf("[claude] finished in %v, stdout=%d bytes, stderr=%d bytes", elapsed, stdout.Len(), stderr.Len())
	if stderr.Len() > 0 {
		log.Printf("[claude] stderr: %s", stderr.String())
	}

	var resp ClaudeResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		log.Printf("[claude] failed to parse JSON: %v", err)
		log.Printf("[claude] raw stdout: %.500s", stdout.String())
		return nil, fmt.Errorf("failed to parse claude response: %v\nraw: %s", err, stdout.String())
	}

	log.Printf("[claude] response: type=%s session=%s isError=%v resultLen=%d",
		resp.Type, resp.SessionID, resp.IsError, len(resp.Result))
	log.Printf("[claude] cost=$%.4f tokens(in=%d out=%d cacheRead=%d cacheCreate=%d) turns=%d duration=%dms",
		resp.CostUSD, resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens,
		resp.NumTurns, resp.DurationMs)
	// Log first 300 chars of result for debugging.
	if len(resp.Result) > 0 {
		preview := resp.Result
		if len(preview) > 300 {
			preview = preview[:300] + "..."
		}
		log.Printf("[claude] result preview: %s", preview)
	}

	if resp.IsError {
		log.Printf("[claude] error response: %s", resp.Result)
		return &resp, fmt.Errorf("claude error: %s", resp.Result)
	}

	return &resp, nil
}

// ExecuteCommand runs a shell command and returns combined stdout+stderr.
// Commands are checked against safeguard rules before execution.
func (c *ClaudeClient) ExecuteCommand(ctx context.Context, command string) (string, error) {
	if verdict, reason := c.safeguard.Check(command); verdict == CommandBlocked {
		log.Printf("[exec] BLOCKED: %s — %s", command, reason)
		return "", fmt.Errorf("command blocked: %s", reason)
	}

	log.Printf("[exec] running: %s", command)
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = c.workDir

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	output := out.String()

	const maxOutput = 10000
	if len(output) > maxOutput {
		log.Printf("[exec] output truncated from %d to %d bytes", len(output), maxOutput)
		output = output[:maxOutput] + "\n... (output truncated)"
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("[exec] timed out after %v", elapsed)
			return output, fmt.Errorf("command timed out")
		}
		log.Printf("[exec] failed after %v: %v (output=%d bytes)", elapsed, err, len(output))
		return output, fmt.Errorf("exit status: %v", err)
	}
	log.Printf("[exec] success in %v, output=%d bytes", elapsed, len(output))
	return output, nil
}

// ParseCommands extracts <command>...</command> blocks from Claude's response.
// Returns the cleaned text (tags replaced with inline code) and the list of commands.
func ParseCommands(text string) (cleanText string, commands []string) {
	matches := commandTagRe.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		cmd := strings.TrimSpace(m[1])
		if cmd != "" {
			commands = append(commands, cmd)
		}
	}

	// Replace <command> tags with inline code for display.
	cleanText = commandTagRe.ReplaceAllStringFunc(text, func(match string) string {
		sub := commandTagRe.FindStringSubmatch(match)
		return "`" + strings.TrimSpace(sub[1]) + "`"
	})
	cleanText = strings.TrimSpace(cleanText)
	return
}

// loginURLRe matches URLs in claude login output.
var loginURLRe = regexp.MustCompile(`https://\S+`)

// ansiRe strips ANSI escape sequences (CSI, OSC, charset, and single-char escapes like save/restore cursor).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b[()#][A-Za-z0-9]|\x1b[A-Za-z0-9=>]`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// IsNotLoggedIn checks if an error indicates Claude CLI is not authenticated.
func IsNotLoggedIn(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Not logged in") || strings.Contains(msg, "not logged in")
}

// SetupToken starts `claude login` inside a PTY so that Ink (the TUI
// framework) gets the TTY it requires. It captures the OAuth URL from output
// and returns the URL plus a feedCode function. Call feedCode with the auth
// code the user receives after completing OAuth in their browser.
// `claude login` stores credentials in ~/.claude/ config so subsequent
// `claude -p` calls are automatically authenticated.
func (c *ClaudeClient) SetupToken(ctx context.Context) (string, func(code string) error, error) {
	log.Printf("[login] starting claude login (with PTY)")
	cmd := exec.CommandContext(ctx, c.claudePath, "login")
	cmd.Dir = c.workDir
	// Prevent browser launch in container.
	cmd.Env = append(os.Environ(), "BROWSER=", "DISPLAY=")

	// Allocate a PTY — wide columns prevent URL line-wrapping.
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 500})
	if err != nil {
		return "", nil, fmt.Errorf("start setup-token with pty: %w", err)
	}

	// drainPTY reads raw bytes from ptmx so the subprocess never blocks on
	// write. Uses raw Read instead of bufio.Scanner because Ink's raw-mode
	// output has no newlines — a line-based scanner would buffer until 64 KB
	// and then error, causing a deadlock.
	drainDone := make(chan struct{})
	drainPTY := func() {
		defer close(drainDone)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				log.Printf("[login] output: %s", stripANSI(string(buf[:n])))
			}
			if err != nil {
				return
			}
		}
	}

	// Auto-advance through the first-time onboarding wizard (theme selection,
	// etc.) by pressing Enter every 2 seconds until we find the OAuth URL.
	urlFound := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-urlFound:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				ptmx.Write([]byte("\r"))
				log.Printf("[login] auto-advancing wizard (sent Enter)")
			}
		}
	}()

	// Read PTY output looking for URL with a timeout.
	// The URL may wrap across multiple lines if it exceeds the terminal width,
	// so we accumulate continuation lines (non-empty, no spaces) after the
	// initial https:// match.
	urlCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(ptmx)
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<20) // 1 MB max to avoid ErrTooLong
		var urlAccum string
		for scanner.Scan() {
			raw := scanner.Text()
			line := stripANSI(raw)
			trimmed := strings.TrimSpace(line)
			log.Printf("[login] output: %s", line)

			if urlAccum != "" {
				// Accumulating URL that wrapped across lines.
				if trimmed != "" && !strings.ContainsAny(trimmed, " \t") {
					urlAccum += trimmed
					continue
				}
				// Hit empty/non-URL line — URL is complete.
				close(urlFound)
				urlCh <- urlAccum
				go drainPTY()
				return
			}

			if u := loginURLRe.FindString(trimmed); u != "" {
				// If URL reaches end of line it may continue on the next line.
				if strings.HasSuffix(trimmed, u[len(u)-1:]) && strings.Index(trimmed, u)+len(u) >= len(trimmed) {
					urlAccum = u
					continue
				}
				close(urlFound)
				urlCh <- u
				go drainPTY()
				return
			}
		}
		close(urlFound)
		if urlAccum != "" {
			urlCh <- urlAccum
			return
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			log.Printf("[login] scanner error: %v", err)
		}
		urlCh <- ""
	}()

	select {
	case loginURL := <-urlCh:
		if loginURL == "" {
			ptmx.Close()
			cmd.Process.Kill()
			cmd.Wait()
			return "", nil, fmt.Errorf("no login URL found in output")
		}
		log.Printf("[login] got URL: %s", loginURL)

		feedCode := func(code string) error {
			log.Printf("[login] feeding auth code (%d chars)", len(code))
			// Write code one character at a time with small delays to
			// simulate real keystrokes. Ink's raw-mode input handler may
			// not correctly process a bulk write of all characters at once.
			for i, ch := range code {
				if _, err := ptmx.Write([]byte(string(ch))); err != nil {
					log.Printf("[login] failed to write char %d to pty: %v", i, err)
					ptmx.Close()
					cmd.Process.Kill()
					cmd.Wait()
					return fmt.Errorf("failed to send code: %w", err)
				}
				time.Sleep(5 * time.Millisecond)
			}
			// Pause before Enter so Ink finishes processing the input.
			time.Sleep(200 * time.Millisecond)
			log.Printf("[login] sending Enter")
			if _, err := ptmx.Write([]byte("\r")); err != nil {
				log.Printf("[login] failed to write Enter to pty: %v", err)
				ptmx.Close()
				cmd.Process.Kill()
				cmd.Wait()
				return fmt.Errorf("failed to send Enter: %w", err)
			}

			// Wait for process with timeout — the subprocess may hang if the
			// token exchange fails or Ink gets stuck.
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()

			// Auto-advance any post-auth prompts (org selection, confirmation, etc.)
			// by pressing Enter every 2 seconds until the process exits.
			stopAdvance := make(chan struct{})
			go func() {
				ticker := time.NewTicker(2 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-stopAdvance:
						return
					case <-ctx.Done():
						return
					case <-ticker.C:
						ptmx.Write([]byte("\r"))
						log.Printf("[login] auto-advancing post-auth prompt (sent Enter)")
					}
				}
			}()

			select {
			case err := <-done:
				close(stopAdvance)
				ptmx.Close()
				// Wait for drain goroutine to finish capturing all output.
				<-drainDone
				if err != nil {
					log.Printf("[login] login exited with: %v", err)
					return fmt.Errorf("login failed: %w", err)
				}
				log.Printf("[login] login completed successfully")
				return nil
			case <-time.After(30 * time.Second):
				close(stopAdvance)
				log.Printf("[login] process didn't exit in 30s, killing and verifying...")
				ptmx.Close()
				cmd.Process.Kill()
				<-done

				// The TUI often hangs on post-auth screens even after creds
				// are saved. Verify login by attempting a quick Claude call.
				verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 15*time.Second)
				_, verifyErr := c.Send(verifyCtx, 0, "", "hi")
				verifyCancel()
				if verifyErr != nil && IsNotLoggedIn(verifyErr) {
					return fmt.Errorf("login timed out (auth may have failed)")
				}
				log.Printf("[login] login verified despite process timeout")
				return nil
			}
		}
		return loginURL, feedCode, nil

	case <-time.After(30 * time.Second):
		ptmx.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return "", nil, fmt.Errorf("timeout waiting for login URL")

	case <-ctx.Done():
		ptmx.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return "", nil, ctx.Err()
	}
}

// FormatCommandResults formats the results of approved/denied commands
// to send back to Claude for context.
func FormatCommandResults(results []CommandResult) string {
	var b strings.Builder
	b.WriteString("Command results:\n\n")
	for i, r := range results {
		fmt.Fprintf(&b, "Command %d: %s\n", i+1, r.Command)
		if r.Approved {
			fmt.Fprintf(&b, "Status: Executed\nOutput:\n%s\n\n", r.Output)
		} else {
			b.WriteString("Status: Denied by user\n\n")
		}
	}
	return b.String()
}
