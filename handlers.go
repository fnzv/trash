package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ProviderStore is a thread-safe map of chatID → active provider ("claude"|"gemini").
type ProviderStore struct {
	mu       sync.RWMutex
	defaults string
	m        map[int64]string
}

func NewProviderStore(defaultProvider string) *ProviderStore {
	if defaultProvider == "" {
		defaultProvider = "claude"
	}
	return &ProviderStore{defaults: defaultProvider, m: make(map[int64]string)}
}

func (p *ProviderStore) Get(chatID int64) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if v, ok := p.m[chatID]; ok {
		return v
	}
	return p.defaults
}

func (p *ProviderStore) Set(chatID int64, provider string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[chatID] = provider
}

func (p *ProviderStore) Delete(chatID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, chatID)
}

// Handlers processes Telegram commands and messages.
type Handlers struct {
	sender         *Sender
	claude         *ClaudeClient
	gemini         *GeminiClient
	sessions       *SessionManager
	geminiSessions *GeminiSessionStore
	providers      *ProviderStore
	approvals      *ApprovalStore
	logins         *LoginStore
	usage          *UsageTracker
	media          *MediaHandler
	locks          *ChatLocks
	allowed        map[int64]bool
	timeout        time.Duration
	skipPerms      bool
	maxRounds      int
}

// ChatLocks manages per-chat mutexes.
type ChatLocks struct {
	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

func NewChatLocks() *ChatLocks {
	return &ChatLocks{locks: make(map[int64]*sync.Mutex)}
}

// Lock acquires the lock for a chatID and returns the unlock function.
func (c *ChatLocks) Lock(chatID int64) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks == nil {
		c.locks = make(map[int64]*sync.Mutex)
	}
	l, exists := c.locks[chatID]
	if !exists {
		l = &sync.Mutex{}
		c.locks[chatID] = l
	}
	l.Lock()
	return l.Unlock
}

func NewHandlers(sender *Sender, claude *ClaudeClient, gemini *GeminiClient, sessions *SessionManager, geminiSessions *GeminiSessionStore, providers *ProviderStore, approvals *ApprovalStore, logins *LoginStore, usage *UsageTracker, media *MediaHandler, cfg *Config) *Handlers {
	return &Handlers{
		sender:         sender,
		claude:         claude,
		gemini:         gemini,
		sessions:       sessions,
		geminiSessions: geminiSessions,
		providers:      providers,
		approvals:      approvals,
		logins:         logins,
		usage:          usage,
		media:          media,
		locks:          NewChatLocks(),
		allowed:        cfg.AllowedChatIDs,
		timeout:        cfg.CommandTimeout,
		skipPerms:      cfg.SkipPermissions,
		maxRounds:      cfg.MaxToolRounds,
	}
}

// IsAllowed checks if a chat ID is in the whitelist.
func (h *Handlers) IsAllowed(chatID int64) bool {
	return h.allowed[chatID]
}

func (h *Handlers) HandleStart(chatID int64) {
	h.sender.SendPlain(chatID,
		"Welcome to AI Code Bot!\n\n"+
			"Send me any message and I'll forward it to Claude (default) or Gemini.\n"+
			"Commands will require your approval before executing.\n"+
			"Use /new to start a fresh conversation, /help for commands, or:\n"+
			"  /claude — switch to Claude\n"+
			"  /gemini — switch to Gemini\n"+
			"  /model  — show active AI")
}

func (h *Handlers) HandleNew(chatID int64) {
	unlock := h.locks.Lock(chatID)
	defer unlock()

	log.Printf("[chat %d] session reset", chatID)
	h.sessions.Delete(chatID)
	h.geminiSessions.Delete(chatID)
	h.approvals.Delete(chatID)
	h.usage.Reset(chatID)
	// Reset Gemini working directory to the configured base.
	h.gemini.mu.Lock()
	h.gemini.cwd = h.gemini.workDir
	h.gemini.mu.Unlock()
	h.sender.SendPlain(chatID, "Session reset. Your next message will start a new conversation.")
}

func (h *Handlers) HandleHelp(chatID int64) {
	h.sender.SendPlain(chatID,
		"AI Code Bot — Commands:\n\n"+
			"/start   - Welcome message\n"+
			"/new     - Reset session (start fresh conversation)\n"+
			"/claude  - Switch active AI to Claude\n"+
			"/gemini  - Switch active AI to Gemini\n"+
			"/model   - Show currently active AI and model\n"+
			"/gmodel  - Switch Gemini model (when using Gemini)\n"+
			"/login   - Login to the active AI (Claude OAuth / Gemini API key)\n"+
			"/usage   - Check usage stats\n"+
			"/safeguard <cmd> - Test a command against safeguard rules\n"+
			"/help    - Show this help message\n\n"+
			"Send any text message and I'll forward it to the active AI. "+
			"When the AI suggests a command, you'll see Approve/Deny buttons. "+
			"Conversation context is maintained until you use /new.")
}

func (h *Handlers) HandleSafeguard(chatID int64, command string) {
	if command == "" {
		h.sender.SendPlain(chatID, "Usage: /safeguard <command>\n\nExample: /safeguard rm -rf /\n\nTests a command against safeguard rules without executing it.")
		return
	}
	verdict, reason := h.claude.safeguard.Check(command)
	if verdict == CommandBlocked {
		h.sender.SendPlain(chatID, fmt.Sprintf("BLOCKED: %s", reason))
	} else {
		h.sender.SendPlain(chatID, fmt.Sprintf("ALLOWED: Command '%s' would pass safeguard checks.", command))
	}
}

func (h *Handlers) HandleUsage(chatID int64) {
	log.Printf("[chat %d] usage command", chatID)

	s := h.usage.Get(chatID)
	if s == nil || s.NumCalls == 0 {
		h.sender.SendPlain(chatID, "No usage data yet. Send some messages first!")
		return
	}

	ago := time.Since(s.LastCallTime).Truncate(time.Second)
	msg := fmt.Sprintf(
		"Session usage:\n"+
			"  Calls: %d\n"+
			"  Input tokens: %d\n"+
			"  Output tokens: %d\n"+
			"  Cost: $%.4f\n"+
			"  Duration: %s\n"+
			"  Last call: %s ago",
		s.NumCalls,
		s.InputTokens,
		s.OutputTokens,
		s.TotalCostUSD,
		s.TotalDuration.Truncate(time.Second),
		ago,
	)
	h.sender.SendPlain(chatID, msg)
}

func (h *Handlers) HandleUnauthorized(chatID int64) {
	log.Printf("WARN: Unauthorized access from chatID %d", chatID)
	h.sender.SendPlain(chatID, fmt.Sprintf("Unauthorized. Your chat ID: %d", chatID))
}

// HandleSwitchProvider switches the active AI provider for a chat and resets the session.
func (h *Handlers) HandleSwitchProvider(chatID int64, provider string) {
	unlock := h.locks.Lock(chatID)
	defer unlock()

	current := h.providers.Get(chatID)
	if current == provider {
		h.sender.SendPlain(chatID, fmt.Sprintf("Already using %s.", provider))
		return
	}

	h.providers.Set(chatID, provider)
	// Reset sessions so the new provider starts fresh.
	h.sessions.Delete(chatID)
	h.geminiSessions.Delete(chatID)
	h.approvals.Delete(chatID)

	log.Printf("[chat %d] switched provider: %s → %s", chatID, current, provider)
	h.sender.SendPlain(chatID, fmt.Sprintf("Switched to %s. Starting a fresh session.", provider))
}

// HandleModel reports the currently active AI provider and model.
func (h *Handlers) HandleModel(chatID int64) {
	provider := h.providers.Get(chatID)
	if provider == "gemini" {
		h.sender.SendPlain(chatID, fmt.Sprintf("Current AI: %s (model: %s)\n\nUse /gmodel to switch Gemini models.", provider, h.gemini.GetModel()))
	} else {
		h.sender.SendPlain(chatID, fmt.Sprintf("Current AI: %s", provider))
	}
}

// geminiModels is the list of available Gemini models shown in /gmodel.
var geminiModels = []struct {
	ID    string
	Label string
}{
	{"gemini-2.5-flash", "⚡ Gemini 2.5 Flash (fast)"},
	{"gemini-2.5-pro", "🧠 Gemini 2.5 Pro (smart)"},
	{"gemini-3-flash-preview", "⚡ Gemini 3 Flash Preview"},
	{"gemini-3-pro-preview", "🧠 Gemini 3 Pro Preview"},
}

// HandleGeminiModel shows an inline keyboard to pick a Gemini model.
func (h *Handlers) HandleGeminiModel(chatID int64) {
	current := h.gemini.GetModel()

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, m := range geminiModels {
		label := m.Label
		if m.ID == current {
			label = "✅ " + label
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, "gmodel:"+m.ID),
		))
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	h.sender.SendWithKeyboard(chatID, fmt.Sprintf("Current Gemini model: `%s`\nChoose a model:", current), keyboard)
}

// HandleMessage processes a user text message.
func (h *Handlers) HandleMessage(ctx context.Context, chatID int64, text string) {
	unlock := h.locks.Lock(chatID)
	defer unlock()

	log.Printf("[chat %d] received message: %s", chatID, text)

	// If there's a pending login, treat this message as the auth code.
	if pending := h.logins.Get(chatID); pending != nil {
		log.Printf("[chat %d] pending login found, treating message as auth code", chatID)
		h.handleLoginCode(ctx, chatID, text, pending)
		return
	}

	if h.approvals.Has(chatID) {
		log.Printf("[chat %d] blocked: pending approval exists", chatID)
		h.sender.SendPlain(chatID, "Please approve or deny the pending command first.")
		return
	}

	h.sender.SendTyping(chatID)
	h.callAI(ctx, chatID, text)
}

// HandlePhoto processes a photo message.
func (h *Handlers) HandlePhoto(ctx context.Context, chatID int64, photos []tgbotapi.PhotoSize, caption string) {
	unlock := h.locks.Lock(chatID)
	defer unlock()

	log.Printf("[chat %d] received photo message", chatID)

	if h.approvals.Has(chatID) {
		h.sender.SendPlain(chatID, "Please approve or deny the pending command first.")
		return
	}

	h.sender.SendTyping(chatID)

	// Pick the largest photo (last in the array).
	photo := photos[len(photos)-1]
	path, err := h.media.DownloadFile(photo.FileID, "jpg")
	if err != nil {
		log.Printf("[chat %d] photo download error: %v", chatID, err)
		h.sender.SendPlain(chatID, fmt.Sprintf("Failed to download photo: %v", err))
		return
	}
	defer h.media.Cleanup(path)

	message := fmt.Sprintf("The user sent an image saved at %s. Please read and analyze it.", path)
	if caption != "" {
		message += fmt.Sprintf("\nUser's message: %s", caption)
	}

	h.callAI(ctx, chatID, message)
}

// HandleVoice processes a voice message.
func (h *Handlers) HandleVoice(ctx context.Context, chatID int64, voice *tgbotapi.Voice, caption string) {
	unlock := h.locks.Lock(chatID)
	defer unlock()

	log.Printf("[chat %d] received voice message", chatID)

	if h.approvals.Has(chatID) {
		h.sender.SendPlain(chatID, "Please approve or deny the pending command first.")
		return
	}

	h.sender.SendTyping(chatID)

	path, err := h.media.DownloadFile(voice.FileID, "ogg")
	if err != nil {
		log.Printf("[chat %d] voice download error: %v", chatID, err)
		h.sender.SendPlain(chatID, fmt.Sprintf("Failed to download voice message: %v", err))
		return
	}
	defer h.media.Cleanup(path)

	transcript, err := h.media.TranscribeAudio(path)
	if err != nil {
		log.Printf("[chat %d] transcription error: %v", chatID, err)
		h.sender.SendPlain(chatID, "Could not transcribe voice message. Make sure whisper is installed.")
		return
	}

	message := fmt.Sprintf("Voice message from user: %s", transcript)
	if caption != "" {
		message += fmt.Sprintf("\nUser's caption: %s", caption)
	}

	h.callAI(ctx, chatID, message)
}

// HandleAudio processes an audio file message.
func (h *Handlers) HandleAudio(ctx context.Context, chatID int64, audio *tgbotapi.Audio, caption string) {
	unlock := h.locks.Lock(chatID)
	defer unlock()

	log.Printf("[chat %d] received audio message", chatID)

	if h.approvals.Has(chatID) {
		h.sender.SendPlain(chatID, "Please approve or deny the pending command first.")
		return
	}

	h.sender.SendTyping(chatID)

	// Determine extension from MIME type.
	ext := "ogg"
	if audio.MimeType != "" {
		parts := strings.Split(audio.MimeType, "/")
		if len(parts) == 2 {
			ext = parts[1]
		}
	}

	path, err := h.media.DownloadFile(audio.FileID, ext)
	if err != nil {
		log.Printf("[chat %d] audio download error: %v", chatID, err)
		h.sender.SendPlain(chatID, fmt.Sprintf("Failed to download audio: %v", err))
		return
	}
	defer h.media.Cleanup(path)

	transcript, err := h.media.TranscribeAudio(path)
	if err != nil {
		log.Printf("[chat %d] transcription error: %v", chatID, err)
		h.sender.SendPlain(chatID, "Could not transcribe audio. Make sure whisper is installed.")
		return
	}

	message := fmt.Sprintf("Audio message from user: %s", transcript)
	if caption != "" {
		message += fmt.Sprintf("\nUser's caption: %s", caption)
	}

	h.callAI(ctx, chatID, message)
}

// HandleLogin starts the login flow for whichever AI provider is currently active.
func (h *Handlers) HandleLogin(ctx context.Context, chatID int64) {
	unlock := h.locks.Lock(chatID)
	defer unlock()

	provider := h.providers.Get(chatID)
	if provider == "gemini" {
		h.performGeminiLogin(ctx, chatID, "")
	} else {
		h.performLogin(ctx, chatID, "")
	}
}

// performGeminiLogin sends the user the Google AI Studio link and waits for them to paste their API key.
func (h *Handlers) performGeminiLogin(ctx context.Context, chatID int64, originalMessage string) {
	// Cancel any existing pending login.
	if old := h.logins.Get(chatID); old != nil {
		log.Printf("[chat %d] cancelling previous pending login", chatID)
		old.Cancel()
		h.logins.Delete(chatID)
	}

	loginCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)

	msg, feedKey, err := h.gemini.SetupToken(loginCtx)
	if err != nil {
		cancel()
		log.Printf("[chat %d] gemini setup-token failed: %v", chatID, err)
		h.sender.SendPlain(chatID, fmt.Sprintf("Gemini login setup failed: %v", err))
		return
	}

	h.logins.Set(chatID, &PendingLogin{
		FeedCode:        feedKey,
		Cancel:          cancel,
		OriginalMessage: originalMessage,
		Provider:        "gemini",
	})

	log.Printf("[chat %d] gemini login: waiting for user to paste API key", chatID)
	h.sender.SendPlain(chatID, msg)
}

// performLogin starts the OAuth login flow via `claude setup-token`.
// Sends the URL to the user and stores state waiting for the auth code.
func (h *Handlers) performLogin(ctx context.Context, chatID int64, originalMessage string) {
	// Cancel any existing pending login to avoid goroutine leaks.
	if old := h.logins.Get(chatID); old != nil {
		log.Printf("[chat %d] cancelling previous pending login", chatID)
		old.Cancel()
		h.logins.Delete(chatID)
	}

	h.sender.SendPlain(chatID, "Claude is not logged in. Starting OAuth login...")

	loginCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)

	url, feedCode, err := h.claude.SetupToken(loginCtx)
	if err != nil {
		cancel()
		log.Printf("[chat %d] setup-token failed: %v", chatID, err)
		h.sender.SendPlain(chatID, fmt.Sprintf("Login failed: %v", err))
		return
	}

	// Store pending login — the next message from this user will be treated as the code.
	h.logins.Set(chatID, &PendingLogin{
		FeedCode:        feedCode,
		Cancel:          cancel,
		OriginalMessage: originalMessage,
		Provider:        "claude",
	})

	log.Printf("[chat %d] login URL obtained, waiting for user to send auth code", chatID)
	h.sender.SendPlain(chatID, fmt.Sprintf(
		"Open this URL to login with your Google account:\n\n%s\n\n"+
			"After authenticating, you'll receive an authorization code.\n"+
			"Paste that code here as your next message.", url))
}

// handleLoginCode processes the auth code/key the user sends during a login flow.
func (h *Handlers) handleLoginCode(ctx context.Context, chatID int64, code string, pending *PendingLogin) {
	h.logins.Delete(chatID)
	defer pending.Cancel()

	code = strings.TrimSpace(code)
	if code == "" {
		h.sender.SendPlain(chatID, "Empty input. Please try again by sending a new message.")
		return
	}

	if pending.Provider == "gemini" {
		log.Printf("[chat %d] verifying Gemini API key", chatID)
		h.sender.SendPlain(chatID, "Verifying API key...")
	} else {
		log.Printf("[chat %d] feeding auth code to setup-token", chatID)
		h.sender.SendPlain(chatID, "Verifying auth code...")
	}

	if err := pending.FeedCode(code); err != nil {
		log.Printf("[chat %d] login error: %v", chatID, err)
		h.sender.SendPlain(chatID, fmt.Sprintf("Login failed: %v\nPlease try again with /login.", err))
		return
	}

	log.Printf("[chat %d] login successful (provider=%s)", chatID, pending.Provider)
	if pending.OriginalMessage == "" {
		providerName := pending.Provider
		if providerName == "" {
			providerName = "Claude"
		}
		h.sender.SendPlain(chatID, fmt.Sprintf("Login successful! You can now send messages to %s.", providerName))
		return
	}
	log.Printf("[chat %d] retrying original message after login", chatID)
	h.sender.SendPlain(chatID, "Login successful! Processing your message...")
	h.sender.SendTyping(chatID)
	h.callAI(ctx, chatID, pending.OriginalMessage)
}

// callAI dispatches to the active AI provider for this chat.
func (h *Handlers) callAI(ctx context.Context, chatID int64, message string) {
	provider := h.providers.Get(chatID)
	log.Printf("[chat %d] callAI: provider=%s", chatID, provider)
	switch provider {
	case "gemini":
		h.callGemini(ctx, chatID, message)
	default:
		h.callClaude(ctx, chatID, message)
	}
}

// callClaude calls the Claude CLI and processes the response.
// If commands are found, shows approval buttons. Otherwise sends text.
func (h *Handlers) callClaude(ctx context.Context, chatID int64, message string) {
	claudeCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	// Typing indicator.
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				h.sender.SendTyping(chatID)
			case <-done:
				return
			}
		}
	}()

	sessionID := h.sessions.Get(chatID)
	if sessionID != "" {
		log.Printf("[chat %d] calling Claude (session=%s)", chatID, sessionID)
	} else {
		log.Printf("[chat %d] calling Claude (new session)", chatID)
	}
	log.Printf("[chat %d] message: %.200s", chatID, message)
	resp, err := h.claude.Send(claudeCtx, chatID, sessionID, message)
	close(done)

	if err != nil {
		if IsNotLoggedIn(err) {
			log.Printf("[chat %d] Claude not logged in, starting OAuth flow", chatID)
			h.performLogin(ctx, chatID, message)
			return
		}
		log.Printf("claude error (chat %d): %v", chatID, err)
		h.sender.SendPlain(chatID, fmt.Sprintf("Error: %v", err))
		return
	}

	// Track usage.
	h.usage.Record(chatID, resp)

	// Update session ID.
	if resp.SessionID != "" {
		log.Printf("[chat %d] session updated: %s", chatID, resp.SessionID)
		h.sessions.Set(chatID, resp.SessionID)
	}

	result := resp.Result
	if result == "" {
		log.Printf("[chat %d] empty response from Claude", chatID)
		h.sender.SendPlain(chatID, "(empty response)")
		return
	}

	log.Printf("[chat %d] response length: %d bytes", chatID, len(result))

	// Parse <command> tags.
	cleanText, commands := ParseCommands(result)
	log.Printf("[chat %d] parsed response: %d commands found, text=%d bytes", chatID, len(commands), len(cleanText))

	// Send the text part to user.
	if cleanText != "" {
		log.Printf("[chat %d] sending text response to user", chatID)
		h.sender.Send(chatID, cleanText)
	}

	// No commands — we're done.
	if len(commands) == 0 {
		log.Printf("[chat %d] no commands, done", chatID)
		return
	}

	for i, cmd := range commands {
		log.Printf("[chat %d] command %d: %s", chatID, i+1, cmd)
	}

	// SKIP_PERMISSIONS: auto-execute all commands.
	if h.skipPerms {
		log.Printf("[chat %d] skip_permissions=true, auto-executing %d commands", chatID, len(commands))
		h.autoExecuteClaude(ctx, chatID, commands, resp.SessionID)
		return
	}

	// Store pending turn and show first approval button.
	turn := &PendingTurn{
		Commands:  commands,
		Results:   make([]CommandResult, 0, len(commands)),
		SessionID: resp.SessionID,
		Provider:  "claude",
	}
	log.Printf("[chat %d] storing %d pending commands, waiting for approval", chatID, len(commands))
	h.approvals.Set(chatID, turn)
	h.showApproval(chatID, turn)
}

// callGemini calls the Gemini CLI and processes the response.
func (h *Handlers) callGemini(ctx context.Context, chatID int64, message string) {
	geminiCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	// Typing indicator.
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				h.sender.SendTyping(chatID)
			case <-done:
				return
			}
		}
	}()

	history := h.geminiSessions.Get(chatID)
	log.Printf("[chat %d] calling Gemini (history turns=%d)", chatID, len(history))
	log.Printf("[chat %d] message: %.200s", chatID, message)

	result, err := h.gemini.Send(geminiCtx, history, message)
	close(done)

	if err != nil {
		if !h.gemini.HasAPIKey() || IsGeminiNotLoggedIn(err) {
			log.Printf("[chat %d] Gemini not authenticated, starting API key flow", chatID)
			h.performGeminiLogin(ctx, chatID, message)
			return
		}
		log.Printf("gemini error (chat %d): %v", chatID, err)
		h.sender.SendPlain(chatID, fmt.Sprintf("Error from Gemini: %v", err))
		return
	}

	// Store conversation turns.
	h.geminiSessions.Append(chatID,
		GeminiMessage{Role: "user", Content: message},
		GeminiMessage{Role: "model", Content: result},
	)

	log.Printf("[chat %d] gemini response length: %d bytes", chatID, len(result))

	// Parse <command> tags.
	cleanText, commands := ParseCommands(result)
	log.Printf("[chat %d] parsed gemini response: %d commands, text=%d bytes", chatID, len(commands), len(cleanText))

	if cleanText != "" {
		h.sender.Send(chatID, cleanText)
	}

	if len(commands) == 0 {
		return
	}

	for i, cmd := range commands {
		log.Printf("[chat %d] gemini command %d: %s", chatID, i+1, cmd)
	}

	// Enforce one command per turn: only take the first command even if Gemini
	// sent multiple. The next command will come after we feed the output back.
	if len(commands) > 1 {
		log.Printf("[chat %d] gemini sent %d commands, trimming to 1", chatID, len(commands))
		commands = commands[:1]
	}

	if h.skipPerms {
		log.Printf("[chat %d] skip_permissions=true, auto-executing %d gemini commands", chatID, len(commands))
		h.autoExecuteGemini(ctx, chatID, commands)
		return
	}

	turn := &PendingTurn{
		Commands:  commands,
		Results:   make([]CommandResult, 0, len(commands)),
		SessionID: "",
		Provider:  "gemini",
	}
	h.approvals.Set(chatID, turn)
	h.showApproval(chatID, turn)
}

// showApproval shows the current pending command with Approve/Deny buttons.
func (h *Handlers) showApproval(chatID int64, turn *PendingTurn) {
	cmd := turn.Commands[turn.CurrentIdx]
	log.Printf("[chat %d] showing approval button %d/%d: %s", chatID, turn.CurrentIdx+1, len(turn.Commands), cmd)
	label := fmt.Sprintf("Command %d/%d:\n`%s`", turn.CurrentIdx+1, len(turn.Commands), cmd)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Approve", "approve"),
			tgbotapi.NewInlineKeyboardButtonData("Deny", "deny"),
		),
	)

	h.sender.SendWithKeyboard(chatID, label, keyboard)
}

// HandleCallback processes Approve/Deny button presses and gmodel selections.
func (h *Handlers) HandleCallback(ctx context.Context, chatID int64, callbackID string, data string, messageID int) {
	unlock := h.locks.Lock(chatID)
	defer unlock()

	// Handle Gemini model selection.
	if strings.HasPrefix(data, "gmodel:") {
		modelID := strings.TrimPrefix(data, "gmodel:")
		h.gemini.SetModel(modelID)
		// Reset session so next message uses the new model fresh.
		h.geminiSessions.Delete(chatID)
		log.Printf("[chat %d] gemini model switched to %s", chatID, modelID)
		h.sender.AnswerCallback(callbackID, "Model switched!")
		h.sender.EditRemoveKeyboard(chatID, messageID, fmt.Sprintf("✅ Switched to `%s`\nSession reset — next message starts fresh.", modelID))
		return
	}

	turn := h.approvals.Get(chatID)
	if turn == nil {
		log.Printf("[chat %d] callback with no pending turn, ignoring", chatID)
		h.sender.AnswerCallback(callbackID, "No pending command.")
		return
	}

	cmd := turn.Commands[turn.CurrentIdx]
	approved := data == "approve"
	log.Printf("[chat %d] callback: command '%s' -> %s", chatID, cmd, data)

	if approved {
		h.sender.AnswerCallback(callbackID, "Approved")
		h.sender.EditRemoveKeyboard(chatID, messageID, fmt.Sprintf("Approved: %s", cmd))

		log.Printf("[chat %d] executing approved command: %s", chatID, cmd)
		h.sender.SendTyping(chatID)

		var output string
		var err error
		if turn.Provider == "gemini" {
			output, err = h.gemini.ExecuteCommand(ctx, cmd)
		} else {
			output, err = h.claude.ExecuteCommand(ctx, cmd)
		}
		if err != nil {
			log.Printf("[chat %d] command error: %v", chatID, err)
			output = fmt.Sprintf("%s\nError: %v", output, err)
		}
		if output == "" {
			output = "(no output)"
		}
		log.Printf("[chat %d] command output: %d bytes", chatID, len(output))

		// Show command output to user.
		display := output
		if len(display) > 2000 {
			display = display[:2000] + "\n... (truncated in chat)"
		}
		h.sender.Send(chatID, display)

		turn.Results = append(turn.Results, CommandResult{
			Command:  cmd,
			Approved: true,
			Output:   output,
		})
	} else {
		log.Printf("[chat %d] command denied: %s", chatID, cmd)
		h.sender.AnswerCallback(callbackID, "Denied")
		h.sender.EditRemoveKeyboard(chatID, messageID, fmt.Sprintf("Denied: %s", cmd))

		turn.Results = append(turn.Results, CommandResult{
			Command:  cmd,
			Approved: false,
		})
	}

	turn.CurrentIdx++

	// More commands in this turn — show next.
	if turn.CurrentIdx < len(turn.Commands) {
		log.Printf("[chat %d] more commands pending (%d/%d)", chatID, turn.CurrentIdx+1, len(turn.Commands))
		h.showApproval(chatID, turn)
		return
	}

	// All commands processed. Send results back to the AI.
	log.Printf("[chat %d] all %d commands processed, sending results back to AI", chatID, len(turn.Results))
	h.approvals.Delete(chatID)
	resultsMsg := FormatCommandResults(turn.Results)

	h.sender.SendTyping(chatID)
	if turn.Provider == "gemini" {
		h.callGemini(ctx, chatID, resultsMsg)
	} else {
		h.callClaude(ctx, chatID, resultsMsg)
	}
}

// autoExecuteClaude runs all commands without approval (SKIP_PERMISSIONS mode, Claude)
// and feeds results back to Claude, looping up to maxRounds.
func (h *Handlers) autoExecuteClaude(ctx context.Context, chatID int64, commands []string, sessionID string) {
	for round := 0; round < h.maxRounds; round++ {
		log.Printf("[chat %d] auto-execute claude round %d: %d commands", chatID, round+1, len(commands))
		var results []CommandResult
		for i, cmd := range commands {
			log.Printf("[chat %d] auto-executing command %d/%d: %s", chatID, i+1, len(commands), cmd)
			h.sender.SendPlain(chatID, fmt.Sprintf("Running: %s", cmd))

			output, err := h.claude.ExecuteCommand(ctx, cmd)
			if err != nil {
				log.Printf("[chat %d] command error: %v", chatID, err)
				output = fmt.Sprintf("%s\nError: %v", output, err)
			}
			if output == "" {
				output = "(no output)"
			}
			log.Printf("[chat %d] command output: %d bytes", chatID, len(output))

			display := output
			if len(display) > 1000 {
				display = display[:1000] + "\n... (truncated)"
			}
			h.sender.Send(chatID, display)

			results = append(results, CommandResult{
				Command:  cmd,
				Approved: true,
				Output:   output,
			})
		}

		// Send results back to Claude.
		log.Printf("[chat %d] sending %d results back to Claude", chatID, len(results))
		resultsMsg := FormatCommandResults(results)
		h.sender.SendTyping(chatID)

		claudeCtx, cancel := context.WithTimeout(ctx, h.timeout)
		sid := h.sessions.Get(chatID)
		resp, err := h.claude.Send(claudeCtx, chatID, sid, resultsMsg)
		cancel()

		if err != nil {
			log.Printf("[chat %d] claude error: %v", chatID, err)
			h.sender.SendPlain(chatID, fmt.Sprintf("Error: %v", err))
			return
		}

		h.usage.Record(chatID, resp)

		if resp.SessionID != "" {
			h.sessions.Set(chatID, resp.SessionID)
		}

		result := resp.Result
		if result == "" {
			log.Printf("[chat %d] empty response, auto-execute done", chatID)
			return
		}

		cleanText, newCommands := ParseCommands(result)
		log.Printf("[chat %d] auto-execute: %d new commands from Claude", chatID, len(newCommands))
		if cleanText != "" {
			h.sender.Send(chatID, cleanText)
		}

		if len(newCommands) == 0 {
			log.Printf("[chat %d] no more commands, auto-execute done", chatID)
			return
		}

		commands = newCommands
		sessionID = resp.SessionID
	}

	log.Printf("[chat %d] hit max tool rounds (%d), stopping", chatID, h.maxRounds)
	h.sender.SendPlain(chatID, "Stopped: too many command rounds.")
}

// autoExecuteGemini runs all commands without approval (SKIP_PERMISSIONS mode, Gemini)
// and feeds results back to Gemini, looping up to maxRounds.
func (h *Handlers) autoExecuteGemini(ctx context.Context, chatID int64, commands []string) {
	for round := 0; round < h.maxRounds; round++ {
		log.Printf("[chat %d] auto-execute gemini round %d: %d commands", chatID, round+1, len(commands))
		var results []CommandResult
		for i, cmd := range commands {
			log.Printf("[chat %d] auto-executing gemini command %d/%d: %s", chatID, i+1, len(commands), cmd)
			h.sender.SendPlain(chatID, fmt.Sprintf("Running: %s", cmd))

			output, err := h.gemini.ExecuteCommand(ctx, cmd)
			if err != nil {
				log.Printf("[chat %d] command error: %v", chatID, err)
				output = fmt.Sprintf("%s\nError: %v", output, err)
			}
			if output == "" {
				output = "(no output)"
			}
			log.Printf("[chat %d] command output: %d bytes", chatID, len(output))

			display := output
			if len(display) > 1000 {
				display = display[:1000] + "\n... (truncated)"
			}
			h.sender.Send(chatID, display)

			results = append(results, CommandResult{
				Command:  cmd,
				Approved: true,
				Output:   output,
			})
		}

		// Send results back to Gemini.
		log.Printf("[chat %d] sending %d results back to Gemini", chatID, len(results))
		resultsMsg := FormatCommandResults(results)
		h.sender.SendTyping(chatID)

		geminiCtx, cancel := context.WithTimeout(ctx, h.timeout)
		history := h.geminiSessions.Get(chatID)
		result, err := h.gemini.Send(geminiCtx, history, resultsMsg)
		cancel()

		if err != nil {
			log.Printf("[chat %d] gemini error: %v", chatID, err)
			h.sender.SendPlain(chatID, fmt.Sprintf("Error from Gemini: %v", err))
			return
		}

		// Store turns.
		h.geminiSessions.Append(chatID,
			GeminiMessage{Role: "user", Content: resultsMsg},
			GeminiMessage{Role: "model", Content: result},
		)

		cleanText, newCommands := ParseCommands(result)
		log.Printf("[chat %d] auto-execute gemini: %d new commands", chatID, len(newCommands))
		if cleanText != "" {
			h.sender.Send(chatID, cleanText)
		}

		if len(newCommands) == 0 {
			log.Printf("[chat %d] no more gemini commands, auto-execute done", chatID)
			return
		}

		commands = newCommands
	}

	log.Printf("[chat %d] hit max tool rounds (%d), stopping", chatID, h.maxRounds)
	h.sender.SendPlain(chatID, "Stopped: too many command rounds.")
}
