package telegram

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestIsDirectedAtBot_SendToShortAliasesRequireExplicitTarget(t *testing.T) {
	oldBot := &Platform{bot: &tgbotapi.BotAPI{Self: tgbotapi.User{ID: 1, UserName: "oldbot"}}}
	newBot := &Platform{bot: &tgbotapi.BotAPI{Self: tgbotapi.User{ID: 2, UserName: "newbot"}}}

	if oldBot.isDirectedAtBot(telegramCommandMessage("/st continue")) {
		t.Fatal("bare /st should not be accepted as a broadcast command")
	}
	if newBot.isDirectedAtBot(telegramCommandMessage("/sd continue")) {
		t.Fatal("bare /sd should not be accepted without an explicit target")
	}
	if oldBot.isDirectedAtBot(telegramCommandMessage("/st @newbot continue")) {
		t.Fatal("old bot should ignore /st targeted at newbot")
	}
	if !newBot.isDirectedAtBot(telegramCommandMessage("/st @newbot continue")) {
		t.Fatal("new bot should accept /st targeted via first @mention argument")
	}
	if oldBot.isDirectedAtBot(telegramCommandMessage("/sd@newbot continue")) {
		t.Fatal("old bot should ignore /sd@newbot")
	}
	if !newBot.isDirectedAtBot(telegramCommandMessage("/sd@newbot continue")) {
		t.Fatal("new bot should accept /sd@newbot")
	}
}

func TestIsTelegramCrossBotCommand_RemovesFwdAlias(t *testing.T) {
	if !isTelegramCrossBotCommand("st") {
		t.Fatal("st should be treated as a cross-bot command")
	}
	if !isTelegramCrossBotCommand("sd") {
		t.Fatal("sd should be treated as a cross-bot command")
	}
	if isTelegramCrossBotCommand("fwd") {
		t.Fatal("fwd should no longer be treated as a cross-bot command")
	}
}
