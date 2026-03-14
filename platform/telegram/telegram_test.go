package telegram

import (
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func resetTelegramReplyRegistry() {
	telegramReplyRegistry.mu.Lock()
	telegramReplyRegistry.byChat = make(map[int64]map[int]storedReplyText)
	telegramReplyRegistry.mu.Unlock()

	telegramIncomingReplyRegistry.mu.Lock()
	defer telegramIncomingReplyRegistry.mu.Unlock()
	telegramIncomingReplyRegistry.byChat = make(map[int64]map[int]storedIncomingReplyReference)
}

func telegramCommandMessage(text string) *tgbotapi.Message {
	return &tgbotapi.Message{
		MessageID: 501,
		Text:      text,
		Chat:      &tgbotapi.Chat{ID: 12345, Type: "group"},
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: len(strings.Fields(text)[0])},
		},
	}
}

func TestResolveReplyText_FromGroupedChunks(t *testing.T) {
	resetTelegramReplyRegistry()

	const chatID int64 = 12345
	fullText := "This is the full assistant response spanning multiple Telegram chunks."
	storeTelegramReplyGroup(chatID, []int{101, 102, 103}, fullText)

	p := &Platform{}
	for _, repliedID := range []string{"101", "102", "103"} {
		got, ok := p.ResolveReplyText(replyContext{chatID: chatID}, repliedID)
		if !ok {
			t.Fatalf("ResolveReplyText(%s) reported not found", repliedID)
		}
		if got != fullText {
			t.Fatalf("ResolveReplyText(%s) = %q, want %q", repliedID, got, fullText)
		}
	}
}

func TestResolveReplyText_ExpiredEntry(t *testing.T) {
	resetTelegramReplyRegistry()

	const chatID int64 = 999
	telegramReplyRegistry.mu.Lock()
	telegramReplyRegistry.byChat[chatID] = map[int]storedReplyText{
		77: {fullText: "expired", expiresAt: time.Now().Add(-time.Minute)},
	}
	telegramReplyRegistry.mu.Unlock()

	p := &Platform{}
	if got, ok := p.ResolveReplyText(replyContext{chatID: chatID}, "77"); ok || got != "" {
		t.Fatalf("ResolveReplyText expired = (%q, %v), want empty false", got, ok)
	}
}

func TestTelegramReplyHelpers_ReadReplyMetadata(t *testing.T) {
	msg := &tgbotapi.Message{
		MessageID: 200,
		ReplyToMessage: &tgbotapi.Message{
			MessageID: 123,
			Text:      "chunk text",
		},
	}

	if got := telegramReplyToMessageID(msg); got != "123" {
		t.Fatalf("telegramReplyToMessageID = %q, want 123", got)
	}
	if got := telegramReplyToContent(msg); got != "chunk text" {
		t.Fatalf("telegramReplyToContent = %q, want chunk text", got)
	}
}

func TestTelegramReplyHelpers_FallbackToCaption(t *testing.T) {
	msg := &tgbotapi.Message{
		ReplyToMessage: &tgbotapi.Message{
			MessageID: 321,
			Caption:   "photo caption",
		},
	}

	if got := telegramReplyToContent(msg); got != "photo caption" {
		t.Fatalf("telegramReplyToContent caption = %q, want photo caption", got)
	}
}

func TestIsDirectedAtBot_SendToRequiresExplicitTarget(t *testing.T) {
	oldBot := &Platform{bot: &tgbotapi.BotAPI{Self: tgbotapi.User{ID: 1, UserName: "oldbot"}}}
	newBot := &Platform{bot: &tgbotapi.BotAPI{Self: tgbotapi.User{ID: 2, UserName: "newbot"}}}

	if oldBot.isDirectedAtBot(telegramCommandMessage("/sendto continue")) {
		t.Fatal("bare /sendto should not be accepted as a broadcast command")
	}
	if newBot.isDirectedAtBot(telegramCommandMessage("/sendto continue")) {
		t.Fatal("bare /sendto should not be accepted without an explicit target")
	}
	if oldBot.isDirectedAtBot(telegramCommandMessage("/sendto @newbot continue")) {
		t.Fatal("old bot should ignore /sendto targeted at newbot")
	}
	if !newBot.isDirectedAtBot(telegramCommandMessage("/sendto @newbot continue")) {
		t.Fatal("new bot should accept /sendto targeted via first @mention argument")
	}
	if oldBot.isDirectedAtBot(telegramCommandMessage("/sendto@newbot continue")) {
		t.Fatal("old bot should ignore /sendto@newbot")
	}
	if !newBot.isDirectedAtBot(telegramCommandMessage("/sendto@newbot continue")) {
		t.Fatal("new bot should accept /sendto@newbot")
	}
}

func TestTelegramReplyHelpers_FallbackToIncomingReplyRegistry(t *testing.T) {
	resetTelegramReplyRegistry()

	storeTelegramIncomingReplyReference(&tgbotapi.Message{
		MessageID: 900,
		Chat:      &tgbotapi.Chat{ID: 777},
		ReplyToMessage: &tgbotapi.Message{
			MessageID: 42,
			Text:      "forward this answer",
		},
	})

	msg := telegramCommandMessage("/sendto@newbot continue")
	msg.Chat.ID = 777
	msg.MessageID = 900

	if got := telegramReplyToMessageID(msg); got != "42" {
		t.Fatalf("telegramReplyToMessageID fallback = %q, want 42", got)
	}
	if got := telegramReplyToContent(msg); got != "forward this answer" {
		t.Fatalf("telegramReplyToContent fallback = %q, want forwarded text", got)
	}
}

func TestTelegramReplyHelpers_WaitForIncomingReplyRegistry(t *testing.T) {
	resetTelegramReplyRegistry()

	msg := telegramCommandMessage("/sendto@newbot continue")
	msg.Chat.ID = 888
	msg.MessageID = 901

	go func() {
		time.Sleep(20 * time.Millisecond)
		storeTelegramIncomingReplyReference(&tgbotapi.Message{
			MessageID: 901,
			Chat:      &tgbotapi.Chat{ID: 888},
			ReplyToMessage: &tgbotapi.Message{
				MessageID: 73,
				Text:      "late reply metadata",
			},
		})
	}()

	if got := telegramReplyToMessageID(msg); got != "73" {
		t.Fatalf("telegramReplyToMessageID wait = %q, want 73", got)
	}
	if got := telegramReplyToContent(msg); got != "late reply metadata" {
		t.Fatalf("telegramReplyToContent wait = %q, want late reply metadata", got)
	}
}
