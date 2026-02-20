package main

import (
	"context"
	"log"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Bot ties together the Telegram API, AI clients, and handlers.
type Bot struct {
	api      *tgbotapi.BotAPI
	handlers *Handlers
}

func NewBot(cfg *Config) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		return nil, err
	}

	log.Printf("Authorized as @%s", api.Self.UserName)

	sender := NewSender(api, []string{cfg.TelegramToken})
	claude := NewClaudeClient(cfg)
	gemini := NewGeminiClient(cfg)
	sessions := NewSessionManager()
	geminiSessions := NewGeminiSessionStore()
	providers := NewProviderStore(cfg.DefaultProvider)
	approvals := NewApprovalStore()
	logins := NewLoginStore()
	usage := NewUsageTracker()
	media := &MediaHandler{api: api, workDir: cfg.WorkDir, whisperCmd: cfg.WhisperCmd}
	handlers := NewHandlers(sender, claude, gemini, sessions, geminiSessions, providers, approvals, logins, usage, media, cfg)

	return &Bot{
		api:      api,
		handlers: handlers,
	}, nil
}

// Run starts the update loop. Blocks until the bot is stopped.
func (b *Bot) Run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	for update := range updates {
		if update.CallbackQuery != nil {
			go b.handleCallback(update)
			continue
		}
		if update.Message == nil {
			continue
		}
		go b.handleUpdate(update)
	}
}

func (b *Bot) handleUpdate(update tgbotapi.Update) {
	log.Printf("Received update %d for chat %d", update.UpdateID, update.Message.Chat.ID)
	msg := update.Message
	chatID := msg.Chat.ID

	// Auth check.
	if !b.handlers.IsAllowed(chatID) {
		b.handlers.HandleUnauthorized(chatID)
		return
	}

	// Command routing.
	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			b.handlers.HandleStart(chatID)
		case "new":
			b.handlers.HandleNew(chatID)
		case "login":
			b.handlers.HandleLogin(context.Background(), chatID)
		case "help":
			b.handlers.HandleHelp(chatID)
		case "usage":
			b.handlers.HandleUsage(chatID)
		case "safeguard":
			b.handlers.HandleSafeguard(chatID, msg.CommandArguments())
		case "gemini":
			b.handlers.HandleSwitchProvider(chatID, "gemini")
		case "claude":
			b.handlers.HandleSwitchProvider(chatID, "claude")
		case "model":
			b.handlers.HandleModel(chatID)
		default:
			b.handlers.HandleHelp(chatID)
		}
		return
	}

	// Media messages.
	if msg.Photo != nil {
		go b.handlers.HandlePhoto(context.Background(), chatID, msg.Photo, msg.Caption)
		return
	}
	if msg.Voice != nil {
		go b.handlers.HandleVoice(context.Background(), chatID, msg.Voice, msg.Caption)
		return
	}
	if msg.Audio != nil {
		go b.handlers.HandleAudio(context.Background(), chatID, msg.Audio, msg.Caption)
		return
	}

	// Text message -> active AI.
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	if text == "" {
		return
	}

	b.handlers.HandleMessage(context.Background(), chatID, text)
}

func (b *Bot) handleCallback(update tgbotapi.Update) {
	cb := update.CallbackQuery
	chatID := cb.Message.Chat.ID
	log.Printf("Received callback %s for chat %d", cb.ID, chatID)

	// Auth check.
	if !b.handlers.IsAllowed(chatID) {
		b.handlers.HandleUnauthorized(chatID)
		return
	}

	b.handlers.HandleCallback(context.Background(), chatID, cb.ID, cb.Data, cb.Message.MessageID)
}
