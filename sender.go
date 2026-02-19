package main

import (
	"log"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const maxMessageLength = 4096

// Sender handles sending messages to Telegram with formatting and splitting.
type Sender struct {
	api    *tgbotapi.BotAPI
	secrets []string // strings to redact from outgoing messages
}

func NewSender(api *tgbotapi.BotAPI, secrets []string) *Sender {
	return &Sender{api: api, secrets: secrets}
}

// redact replaces any secret values in text with "[REDACTED]".
func (s *Sender) redact(text string) string {
	for _, secret := range s.secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	return text
}

// Send sends text to a chat, converting to MarkdownV2 with plain-text fallback.
// Long messages are split at newline/space boundaries.
func (s *Sender) Send(chatID int64, text string) {
	text = s.redact(text)
	chunks := splitMessage(text, maxMessageLength)

	for i, chunk := range chunks {
		formatted := ToTelegramMarkdownV2(chunk)
		msg := tgbotapi.NewMessage(chatID, formatted)
		msg.ParseMode = tgbotapi.ModeMarkdownV2

		_, err := s.api.Send(msg)
		if err != nil {
			log.Printf("MarkdownV2 send failed (chunk %d): %v; falling back to plain text", i, err)
			msg := tgbotapi.NewMessage(chatID, chunk)
			if _, err := s.api.Send(msg); err != nil {
				log.Printf("plain text send also failed (chunk %d): %v", i, err)
			}
		}
	}
}

// SendTyping sends a "typing..." indicator to the chat.
func (s *Sender) SendTyping(chatID int64) {
	action := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	s.api.Send(action)
}

// SendPlain sends a plain text message without any formatting.
func (s *Sender) SendPlain(chatID int64, text string) {
	text = s.redact(text)
	for _, chunk := range splitMessage(text, maxMessageLength) {
		msg := tgbotapi.NewMessage(chatID, chunk)
		if _, err := s.api.Send(msg); err != nil {
			log.Printf("send failed: %v", err)
		}
	}
}

// AnswerCallback acknowledges a callback query with optional text.
func (s *Sender) AnswerCallback(callbackID, text string) {
	cb := tgbotapi.NewCallback(callbackID, text)
	if _, err := s.api.Request(cb); err != nil {
		log.Printf("answer callback failed: %v", err)
	}
}

// SendWithKeyboard sends a message with inline keyboard buttons. Returns the message ID.
func (s *Sender) SendWithKeyboard(chatID int64, text string, keyboard tgbotapi.InlineKeyboardMarkup) int {
	text = s.redact(text)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = keyboard
	msg.ParseMode = tgbotapi.ModeMarkdownV2

	sent, err := s.api.Send(msg)
	if err != nil {
		// Fallback without MarkdownV2
		msg.ParseMode = ""
		sent, err = s.api.Send(msg)
		if err != nil {
			log.Printf("send with keyboard failed: %v", err)
			return 0
		}
	}
	return sent.MessageID
}

// EditRemoveKeyboard edits a message to show new text and removes the inline keyboard.
func (s *Sender) EditRemoveKeyboard(chatID int64, messageID int, newText string) {
	newText = s.redact(newText)
	edit := tgbotapi.NewEditMessageText(chatID, messageID, newText)
	emptyMarkup := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
	edit.ReplyMarkup = &emptyMarkup
	if _, err := s.api.Send(edit); err != nil {
		log.Printf("edit remove keyboard failed: %v", err)
	}
}

// splitMessage splits text into chunks respecting maxLen.
// Prefers splitting at newlines, then spaces, then hard breaks.
func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		splitAt := maxLen
		chunk := text[:maxLen]

		if idx := strings.LastIndex(chunk, "\n"); idx > 0 {
			splitAt = idx + 1
		} else if idx := strings.LastIndex(chunk, " "); idx > 0 {
			splitAt = idx + 1
		}

		chunks = append(chunks, text[:splitAt])
		text = text[splitAt:]
	}
	return chunks
}
