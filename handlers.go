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

// Handlers processes Telegram commands and messages.
type Handlers struct {
	sender    *Sender
	claude    *ClaudeClient
	sessions  *SessionManager
	approvals *ApprovalStore
	logins    *LoginStore
	usage     *UsageTracker
	media     *MediaHandler
	locks     *ChatLocks
	allowed   map[int64]bool
	timeout   time.Duration
	skipPerms bool
	maxRounds int
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

func NewHandlers(sender *Sender, claude *ClaudeClient, sessions *SessionManager, approvals *ApprovalStore, logins *LoginStore, usage *UsageTracker, media *MediaHandler, cfg *Config) *Handlers {
	return &Handlers{
		sender:    sender,
		claude:    claude,
		sessions:  sessions,
		approvals: approvals,
		logins:    logins,
		usage:     usage,
		media:     media,
		locks:     NewChatLocks(),
		allowed:   cfg.AllowedChatIDs,
		timeout:   cfg.CommandTimeout,
		skipPerms: cfg.SkipPermissions,
		maxRounds: cfg.MaxToolRounds,
	}
}

// IsAllowed checks if a chat ID is in the whitelist.
func (h *Handlers) IsAllowed(chatID int64) bool {
	return h.allowed[chatID]
}

func (h *Handlers) HandleStart(chatID int64) {
	h.sender.SendPlain(chatID,
		"Welcome to Claude Code Bot!\n\n"+
			"Send me any message and I'll forward it to Claude.\n"+
			"Commands will require your approval before executing.\n"+
			"Use /new to start a fresh conversation, or /help for more info.")
}

func (h *Handlers) HandleNew(chatID int64) {
	unlock := h.locks.Lock(chatID)
	defer unlock()

	log.Printf("[chat %d] session reset", chatID)
	h.sessions.Delete(chatID)
	h.approvals.Delete(chatID)
	h.usage.Reset(chatID)
	h.sender.SendPlain(chatID, "Session reset. Your next message will start a new conversation.")
}

func (h *Handlers) HandleHelp(chatID int64) {
	h.sender.SendPlain(chatID,
		"Claude Code Bot - Commands:\n\n"+
			"/start - Welcome message\n"+
			"/new - Reset session (start fresh conversation)\n"+
			"/login - Manually start OAuth login\n"+
			"/usage - Check Claude Code plan usage and rate limits\n"+
			"/safeguard <cmd> - Test a command against safeguard rules\n"+
			"/help - Show this help message\n\n"+
			"Send any text message and I'll forward it to Claude. "+
			"When Claude suggests a command, you'll see Approve/Deny buttons. "+
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
	h.callClaude(ctx, chatID, text)
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

	h.callClaude(ctx, chatID, message)
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

	h.callClaude(ctx, chatID, message)
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

	h.callClaude(ctx, chatID, message)
}

func (h *Handlers) HandleLogin(ctx context.Context, chatID int64) {
	unlock := h.locks.Lock(chatID)
	defer unlock()
	h.performLogin(ctx, chatID, "")
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
	})

	log.Printf("[chat %d] login URL obtained, waiting for user to send auth code", chatID)
	h.sender.SendPlain(chatID, fmt.Sprintf(
		"Open this URL to login with your Google account:\n\n%s\n\n"+
			"After authenticating, you'll receive an authorization code.\n"+
			"Paste that code here as your next message.", url))
}

// handleLoginCode processes the auth code the user sends after OAuth.
func (h *Handlers) handleLoginCode(ctx context.Context, chatID int64, code string, pending *PendingLogin) {
	h.logins.Delete(chatID)
	defer pending.Cancel()

	code = strings.TrimSpace(code)
	if code == "" {
		h.sender.SendPlain(chatID, "Empty code. Please try again by sending a new message.")
		return
	}

	log.Printf("[chat %d] feeding auth code to setup-token", chatID)
	h.sender.SendPlain(chatID, "Verifying auth code...")

	if err := pending.FeedCode(code); err != nil {
		log.Printf("[chat %d] setup-token error: %v", chatID, err)
		h.sender.SendPlain(chatID, fmt.Sprintf("Login failed: %v\nPlease try again by sending a new message.", err))
		return
	}

	log.Printf("[chat %d] login successful", chatID)
	if pending.OriginalMessage == "" {
		h.sender.SendPlain(chatID, "Login successful! You can now send messages to Claude.")
		return
	}
	log.Printf("[chat %d] retrying original message", chatID)
	h.sender.SendPlain(chatID, "Login successful! Processing your message...")
	h.sender.SendTyping(chatID)
	h.callClaude(ctx, chatID, pending.OriginalMessage)
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
		h.autoExecute(ctx, chatID, commands, resp.SessionID)
		return
	}

	// Store pending turn and show first approval button.
	turn := &PendingTurn{
		Commands:  commands,
		Results:   make([]CommandResult, 0, len(commands)),
		SessionID: resp.SessionID,
	}
	log.Printf("[chat %d] storing %d pending commands, waiting for approval", chatID, len(commands))
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

// HandleCallback processes Approve/Deny button presses.
func (h *Handlers) HandleCallback(ctx context.Context, chatID int64, callbackID string, data string, messageID int) {
	unlock := h.locks.Lock(chatID)
	defer unlock()

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
		output, err := h.claude.ExecuteCommand(ctx, cmd)
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

	// All commands processed. Send results back to Claude.
	log.Printf("[chat %d] all %d commands processed, sending results back to Claude", chatID, len(turn.Results))
	h.approvals.Delete(chatID)
	resultsMsg := FormatCommandResults(turn.Results)

	h.sender.SendTyping(chatID)
	h.callClaude(ctx, chatID, resultsMsg)
}

// autoExecute runs all commands without approval (SKIP_PERMISSIONS mode)
// and feeds results back to Claude, looping up to maxRounds.
func (h *Handlers) autoExecute(ctx context.Context, chatID int64, commands []string, sessionID string) {
	for round := 0; round < h.maxRounds; round++ {
		log.Printf("[chat %d] auto-execute round %d: %d commands", chatID, round+1, len(commands))
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
