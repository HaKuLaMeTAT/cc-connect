package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/chenhg5/cc-connect/core"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func init() {
	core.RegisterPlatform("telegram", New)
}

type replyContext struct {
	chatID    int64
	messageID int
}

type Platform struct {
	token                 string
	allowFrom             string
	groupReplyAll         bool
	shareSessionInChannel bool
	bot                   *tgbotapi.BotAPI
	httpClient            *http.Client
	handler               core.MessageHandler
	cancel                context.CancelFunc
}

type storedReplyText struct {
	fullText  string
	expiresAt time.Time
}

type storedIncomingReplyReference struct {
	replyToMessageID string
	replyToContent   string
	expiresAt        time.Time
}

var telegramReplyRegistry = struct {
	mu     sync.Mutex
	byChat map[int64]map[int]storedReplyText
}{
	byChat: make(map[int64]map[int]storedReplyText),
}

var telegramIncomingReplyRegistry = struct {
	mu     sync.Mutex
	byChat map[int64]map[int]storedIncomingReplyReference
}{
	byChat: make(map[int64]map[int]storedIncomingReplyReference),
}

const (
	telegramReplyRegistryTTL           = 24 * time.Hour
	telegramIncomingReplyLookupTimeout = 300 * time.Millisecond
	telegramIncomingReplyLookupStep    = 10 * time.Millisecond
)

func New(opts map[string]any) (core.Platform, error) {
	token, _ := opts["token"].(string)
	if token == "" {
		return nil, fmt.Errorf("telegram: token is required")
	}
	allowFrom, _ := opts["allow_from"].(string)

	// Build HTTP client with optional proxy support
	httpClient := &http.Client{Timeout: 60 * time.Second}
	if proxyURL, _ := opts["proxy"].(string); proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("telegram: invalid proxy URL %q: %w", proxyURL, err)
		}
		proxyUser, _ := opts["proxy_username"].(string)
		proxyPass, _ := opts["proxy_password"].(string)
		if proxyUser != "" {
			u.User = url.UserPassword(proxyUser, proxyPass)
		}
		httpClient.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
		slog.Info("telegram: using proxy", "proxy", u.Host, "auth", proxyUser != "")
	}

	groupReplyAll, _ := opts["group_reply_all"].(bool)
	shareSessionInChannel, _ := opts["share_session_in_channel"].(bool)
	return &Platform{token: token, allowFrom: allowFrom, groupReplyAll: groupReplyAll, shareSessionInChannel: shareSessionInChannel, httpClient: httpClient}, nil
}

func (p *Platform) Name() string { return "telegram" }

func (p *Platform) Start(handler core.MessageHandler) error {
	p.handler = handler

	bot, err := tgbotapi.NewBotAPIWithClient(p.token, tgbotapi.APIEndpoint, p.httpClient)
	if err != nil {
		return fmt.Errorf("telegram: auth failed: %w", err)
	}
	p.bot = bot

	slog.Info("telegram: connected", "bot", bot.Self.UserName)

	// Drain pending updates from previous session to avoid re-processing old messages.
	// offset -1 tells Telegram to mark all pending updates as confirmed, returning only the latest one.
	drain := tgbotapi.NewUpdate(-1)
	drain.Timeout = 0
	if _, err := bot.GetUpdates(drain); err != nil {
		slog.Warn("telegram: failed to drain old updates", "error", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := bot.GetUpdatesChan(u)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-updates:
				if !ok {
					return
				}
				// Handle inline keyboard button clicks
				if update.CallbackQuery != nil {
					p.handleCallbackQuery(update.CallbackQuery)
					continue
				}

				if update.Message == nil {
					continue
				}

				msg := update.Message
				msgTime := time.Unix(int64(msg.Date), 0)
				if core.IsOldMessage(msgTime) {
					slog.Debug("telegram: ignoring old message after restart", "date", msgTime)
					continue
				}
				userName := msg.From.UserName
				if userName == "" {
					userName = strings.TrimSpace(msg.From.FirstName + " " + msg.From.LastName)
				}
				var sessionKey string
				if p.shareSessionInChannel {
					sessionKey = fmt.Sprintf("telegram:%d", msg.Chat.ID)
				} else {
					sessionKey = fmt.Sprintf("telegram:%d:%d", msg.Chat.ID, msg.From.ID)
				}
				userID := strconv.FormatInt(msg.From.ID, 10)
				if !core.AllowList(p.allowFrom, userID) {
					slog.Debug("telegram: message from unauthorized user", "user", userID)
					continue
				}
				storeTelegramIncomingReplyReference(msg)

				isGroup := msg.Chat.Type == "group" || msg.Chat.Type == "supergroup"

				// In group chats, filter messages not directed at this bot (unless group_reply_all)
				if isGroup && !p.groupReplyAll {
					slog.Debug("telegram: checking group message", "bot", p.bot.Self.UserName, "text", msg.Text, "is_command", msg.IsCommand())
					if !p.isDirectedAtBot(msg) {
						continue
					}
				}

				rctx := replyContext{chatID: msg.Chat.ID, messageID: msg.MessageID}

				// Handle photo messages
				if msg.Photo != nil && len(msg.Photo) > 0 {
					best := msg.Photo[len(msg.Photo)-1]
					imgData, err := p.downloadFile(best.FileID)
					if err != nil {
						slog.Error("telegram: download photo failed", "error", err)
						continue
					}
					caption := msg.Caption
					if p.bot.Self.UserName != "" {
						caption = strings.ReplaceAll(caption, "@"+p.bot.Self.UserName, "")
						caption = strings.TrimSpace(caption)
					}
					coreMsg := &core.Message{
						SessionKey: sessionKey, Platform: "telegram",
						UserID: userID, UserName: userName,
						Content:          caption,
						MessageID:        strconv.Itoa(msg.MessageID),
						ReplyToMessageID: telegramReplyToMessageID(msg),
						ReplyToContent:   telegramReplyToContent(msg),
						Images:           []core.ImageAttachment{{MimeType: "image/jpeg", Data: imgData}},
						ReplyCtx:         rctx,
					}
					p.handler(p, coreMsg)
					continue
				}

				// Handle voice messages
				if msg.Voice != nil {
					slog.Debug("telegram: voice received", "user", userName, "duration", msg.Voice.Duration)
					audioData, err := p.downloadFile(msg.Voice.FileID)
					if err != nil {
						slog.Error("telegram: download voice failed", "error", err)
						continue
					}
					coreMsg := &core.Message{
						SessionKey: sessionKey, Platform: "telegram",
						UserID: userID, UserName: userName,
						MessageID:        strconv.Itoa(msg.MessageID),
						ReplyToMessageID: telegramReplyToMessageID(msg),
						ReplyToContent:   telegramReplyToContent(msg),
						Audio: &core.AudioAttachment{
							MimeType: msg.Voice.MimeType,
							Data:     audioData,
							Format:   "ogg",
							Duration: msg.Voice.Duration,
						},
						ReplyCtx: rctx,
					}
					p.handler(p, coreMsg)
					continue
				}

				// Handle audio file messages
				if msg.Audio != nil {
					slog.Debug("telegram: audio file received", "user", userName)
					audioData, err := p.downloadFile(msg.Audio.FileID)
					if err != nil {
						slog.Error("telegram: download audio failed", "error", err)
						continue
					}
					format := "mp3"
					if msg.Audio.MimeType != "" {
						parts := strings.SplitN(msg.Audio.MimeType, "/", 2)
						if len(parts) == 2 {
							format = parts[1]
						}
					}
					coreMsg := &core.Message{
						SessionKey: sessionKey, Platform: "telegram",
						UserID: userID, UserName: userName,
						MessageID:        strconv.Itoa(msg.MessageID),
						ReplyToMessageID: telegramReplyToMessageID(msg),
						ReplyToContent:   telegramReplyToContent(msg),
						Audio: &core.AudioAttachment{
							MimeType: msg.Audio.MimeType,
							Data:     audioData,
							Format:   format,
							Duration: msg.Audio.Duration,
						},
						ReplyCtx: rctx,
					}
					p.handler(p, coreMsg)
					continue
				}

				// Handle document/file messages
				if msg.Document != nil {
					slog.Debug("telegram: document received", "user", userName, "file_name", msg.Document.FileName, "mime", msg.Document.MimeType)
					fileData, err := p.downloadFile(msg.Document.FileID)
					if err != nil {
						slog.Error("telegram: download document failed", "error", err)
						continue
					}
					caption := msg.Caption
					if p.bot.Self.UserName != "" {
						caption = strings.ReplaceAll(caption, "@"+p.bot.Self.UserName, "")
						caption = strings.TrimSpace(caption)
					}
					coreMsg := &core.Message{
						SessionKey: sessionKey, Platform: "telegram",
						UserID: userID, UserName: userName,
						Content:          caption,
						MessageID:        strconv.Itoa(msg.MessageID),
						ReplyToMessageID: telegramReplyToMessageID(msg),
						ReplyToContent:   telegramReplyToContent(msg),
						Files:            []core.FileAttachment{{MimeType: msg.Document.MimeType, Data: fileData, FileName: msg.Document.FileName}},
						ReplyCtx:         rctx,
					}
					p.handler(p, coreMsg)
					continue
				}

				if msg.Text == "" {
					continue
				}

				text := msg.Text
				if p.bot.Self.UserName != "" {
					text = strings.ReplaceAll(text, "@"+p.bot.Self.UserName, "")
					text = strings.TrimSpace(text)
				}

				coreMsg := &core.Message{
					SessionKey: sessionKey, Platform: "telegram",
					UserID: userID, UserName: userName,
					Content:          text,
					MessageID:        strconv.Itoa(msg.MessageID),
					ReplyToMessageID: telegramReplyToMessageID(msg),
					ReplyToContent:   telegramReplyToContent(msg),
					ReplyCtx:         rctx,
				}

				slog.Debug("telegram: message received", "user", userName, "chat", msg.Chat.ID)
				p.handler(p, coreMsg)
			}
		}
	}()

	return nil
}

func (p *Platform) handleCallbackQuery(cb *tgbotapi.CallbackQuery) {
	if cb.Message == nil || cb.From == nil {
		return
	}

	data := cb.Data
	chatID := cb.Message.Chat.ID
	msgID := cb.Message.MessageID
	userID := strconv.FormatInt(cb.From.ID, 10)

	if !core.AllowList(p.allowFrom, userID) {
		slog.Debug("telegram: callback from unauthorized user", "user", userID)
		return
	}

	// Answer the callback to clear the loading indicator
	answer := tgbotapi.NewCallback(cb.ID, "")
	p.bot.Request(answer)

	userName := cb.From.UserName
	if userName == "" {
		userName = strings.TrimSpace(cb.From.FirstName + " " + cb.From.LastName)
	}
	var sessionKey string
	if p.shareSessionInChannel {
		sessionKey = fmt.Sprintf("telegram:%d", chatID)
	} else {
		sessionKey = fmt.Sprintf("telegram:%d:%d", chatID, cb.From.ID)
	}
	rctx := replyContext{chatID: chatID, messageID: msgID}

	// Command callbacks (cmd:/lang en, cmd:/mode yolo, etc.)
	if strings.HasPrefix(data, "cmd:") {
		command := strings.TrimPrefix(data, "cmd:")

		// Edit original message: append the chosen option and remove buttons
		origText := cb.Message.Text
		if origText == "" {
			origText = ""
		}
		edit := tgbotapi.NewEditMessageText(chatID, msgID, origText+"\n\n> "+command)
		emptyMarkup := tgbotapi.NewInlineKeyboardMarkup()
		edit.ReplyMarkup = &emptyMarkup
		p.bot.Send(edit)

		p.handler(p, &core.Message{
			SessionKey: sessionKey,
			Platform:   "telegram",
			UserID:     userID,
			UserName:   userName,
			Content:    command,
			MessageID:  strconv.Itoa(msgID),
			ReplyCtx:   rctx,
		})
		return
	}

	// Permission callbacks (perm:allow, perm:deny, perm:allow_all)
	var responseText string
	switch data {
	case "perm:allow":
		responseText = "allow"
	case "perm:deny":
		responseText = "deny"
	case "perm:allow_all":
		responseText = "allow all"
	default:
		slog.Debug("telegram: unknown callback data", "data", data)
		return
	}

	choiceLabel := responseText
	switch data {
	case "perm:allow":
		choiceLabel = "✅ Allowed"
	case "perm:deny":
		choiceLabel = "❌ Denied"
	case "perm:allow_all":
		choiceLabel = "✅ Allow All"
	}

	origText := cb.Message.Text
	if origText == "" {
		origText = "(permission request)"
	}
	edit := tgbotapi.NewEditMessageText(chatID, msgID, origText+"\n\n"+choiceLabel)
	emptyMarkup := tgbotapi.NewInlineKeyboardMarkup()
	edit.ReplyMarkup = &emptyMarkup
	p.bot.Send(edit)

	p.handler(p, &core.Message{
		SessionKey: sessionKey,
		Platform:   "telegram",
		UserID:     userID,
		UserName:   userName,
		Content:    responseText,
		MessageID:  strconv.Itoa(msgID),
		ReplyCtx:   rctx,
	})
}

// isDirectedAtBot checks whether a group message is directed at this bot:
//   - Command with @thisbot suffix (e.g. /help@thisbot)
//   - Most commands without @suffix are broadcast to all bots
//   - Cross-bot relay commands (/sendto, /quote, /st, /sd) require an explicit target
//   - Command with @otherbot suffix → reject
//   - Non-command: accept if bot is @mentioned or message is a reply to bot
func (p *Platform) isDirectedAtBot(msg *tgbotapi.Message) bool {
	botName := p.bot.Self.UserName
	commandText := telegramCommandText(msg)

	// Commands: /cmd or /cmd@botname
	if msg.IsCommand() {
		cmdName := telegramCommandName(commandText)
		if isTelegramCrossBotCommand(cmdName) {
			target, ok := telegramExplicitCommandTarget(commandText)
			if !ok {
				slog.Debug("telegram: cross-bot command without explicit target, rejecting", "bot", botName, "text", commandText)
				return false
			}
			match := strings.EqualFold(target, botName)
			slog.Debug("telegram: cross-bot command target", "bot", botName, "cmd", cmdName, "target", target, "match", match)
			return match
		}

		atIdx := strings.Index(commandText, "@")
		spaceIdx := strings.Index(commandText, " ")
		cmdEnd := len(commandText)
		if spaceIdx > 0 {
			cmdEnd = spaceIdx
		}
		if atIdx > 0 && atIdx < cmdEnd {
			target := commandText[atIdx+1 : cmdEnd]
			slog.Debug("telegram: command with @suffix", "bot", botName, "target", target, "match", strings.EqualFold(target, botName))
			return strings.EqualFold(target, botName)
		}
		slog.Debug("telegram: command without @suffix, accepting", "bot", botName, "text", commandText)
		return true // /cmd without @suffix — accept
	}

	// Non-command: check @mention
	if msg.Entities != nil {
		for _, e := range msg.Entities {
			if e.Type == "mention" {
				mention := extractEntityText(msg.Text, e.Offset, e.Length)
				slog.Debug("telegram: checking mention", "bot", botName, "mention", mention, "match", strings.EqualFold(mention, "@"+botName))
				if strings.EqualFold(mention, "@"+botName) {
					return true
				}
			}
		}
	}

	// Check if replying to a message from this bot
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		slog.Debug("telegram: checking reply", "bot_id", p.bot.Self.ID, "reply_from_id", msg.ReplyToMessage.From.ID)
		if msg.ReplyToMessage.From.ID == p.bot.Self.ID {
			return true
		}
	}

	// Also check caption entities (for photos with captions)
	if msg.CaptionEntities != nil {
		for _, e := range msg.CaptionEntities {
			if e.Type == "mention" {
				mention := extractEntityText(msg.Caption, e.Offset, e.Length)
				if strings.EqualFold(mention, "@"+botName) {
					return true
				}
			}
		}
	}

	slog.Debug("telegram: ignoring group message not directed at bot", "chat", msg.Chat.ID, "bot", botName, "text", msg.Text, "entities", msg.Entities)
	return false
}

func (p *Platform) Reply(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("telegram: invalid reply context type %T", rctx)
	}

	html := core.MarkdownToTelegramHTML(content)
	reply := tgbotapi.NewMessage(rc.chatID, html)
	reply.ReplyToMessageID = rc.messageID
	reply.ParseMode = tgbotapi.ModeHTML

	if _, err := p.bot.Send(reply); err != nil {
		if strings.Contains(err.Error(), "can't parse") {
			reply.Text = content
			reply.ParseMode = ""
			_, err = p.bot.Send(reply)
		}
		if err != nil {
			return fmt.Errorf("telegram: send: %w", err)
		}
	}
	return nil
}

// Send sends a new message (not a reply)
func (p *Platform) Send(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("telegram: invalid reply context type %T", rctx)
	}

	html := core.MarkdownToTelegramHTML(content)
	msg := tgbotapi.NewMessage(rc.chatID, html)
	msg.ParseMode = tgbotapi.ModeHTML

	if _, err := p.bot.Send(msg); err != nil {
		if strings.Contains(err.Error(), "can't parse") {
			msg.Text = content
			msg.ParseMode = ""
			_, err = p.bot.Send(msg)
		}
		if err != nil {
			return fmt.Errorf("telegram: send: %w", err)
		}
	}
	return nil
}

func (p *Platform) SendGrouped(ctx context.Context, rctx any, fullContent string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("telegram: invalid reply context type %T", rctx)
	}

	chunks := splitTelegramMessage(fullContent, 4000)
	var sentIDs []int
	for _, chunk := range chunks {
		sent, err := p.sendTelegramMessage(rc.chatID, chunk, nil, false, 0)
		if err != nil {
			return err
		}
		sentIDs = append(sentIDs, sent.MessageID)
	}
	storeTelegramReplyGroup(rc.chatID, sentIDs, fullContent)
	return nil
}

// SendWithButtons sends a message with an inline keyboard.
func (p *Platform) SendWithButtons(ctx context.Context, rctx any, content string, buttons [][]core.ButtonOption) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("telegram: invalid reply context type %T", rctx)
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, row := range buttons {
		var btns []tgbotapi.InlineKeyboardButton
		for _, b := range row {
			btns = append(btns, tgbotapi.NewInlineKeyboardButtonData(b.Text, b.Data))
		}
		rows = append(rows, btns)
	}

	html := core.MarkdownToTelegramHTML(content)
	msg := tgbotapi.NewMessage(rc.chatID, html)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)

	if _, err := p.bot.Send(msg); err != nil {
		if strings.Contains(err.Error(), "can't parse") {
			msg.Text = content
			msg.ParseMode = ""
			_, err = p.bot.Send(msg)
		}
		if err != nil {
			return fmt.Errorf("telegram: sendWithButtons: %w", err)
		}
	}
	return nil
}

func (p *Platform) ResolveReplyText(rctx any, repliedMessageID string) (string, bool) {
	rc, ok := rctx.(replyContext)
	if !ok {
		return "", false
	}
	msgID, err := strconv.Atoi(strings.TrimSpace(repliedMessageID))
	if err != nil {
		return "", false
	}
	return resolveTelegramReplyGroup(rc.chatID, msgID)
}

// DeletePreviewMessage deletes a stale preview message so the caller can send a fresh one.
func (p *Platform) DeletePreviewMessage(ctx context.Context, previewHandle any) error {
	h, ok := previewHandle.(*telegramPreviewHandle)
	if !ok {
		return fmt.Errorf("telegram: invalid preview handle type %T", previewHandle)
	}
	del := tgbotapi.NewDeleteMessage(h.chatID, h.messageID)
	_, err := p.bot.Request(del)
	if err != nil {
		slog.Debug("telegram: delete preview message failed", "error", err)
	}
	return err
}

func (p *Platform) downloadFile(fileID string) ([]byte, error) {
	fileConfig := tgbotapi.FileConfig{FileID: fileID}
	file, err := p.bot.GetFile(fileConfig)
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}
	link := file.Link(p.bot.Token)

	resp, err := p.httpClient.Get(link)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	// telegram:{chatID}:{userID}
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) < 2 || parts[0] != "telegram" {
		return nil, fmt.Errorf("telegram: invalid session key %q", sessionKey)
	}
	chatID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("telegram: invalid chat ID in %q", sessionKey)
	}
	return replyContext{chatID: chatID}, nil
}

// telegramPreviewHandle stores the chat and message IDs for an editable preview message.
type telegramPreviewHandle struct {
	chatID    int64
	messageID int
}

// SendPreviewStart sends a new message and returns a handle for subsequent edits.
func (p *Platform) SendPreviewStart(ctx context.Context, rctx any, content string) (any, error) {
	rc, ok := rctx.(replyContext)
	if !ok {
		return nil, fmt.Errorf("telegram: invalid reply context type %T", rctx)
	}

	sent, err := p.sendTelegramMessage(rc.chatID, content, nil, false, 0)
	if err != nil {
		return nil, fmt.Errorf("telegram: send preview: %w", err)
	}
	storeTelegramReplyGroup(rc.chatID, []int{sent.MessageID}, content)
	return &telegramPreviewHandle{chatID: rc.chatID, messageID: sent.MessageID}, nil
}

// UpdateMessage edits an existing message identified by previewHandle.
func (p *Platform) UpdateMessage(ctx context.Context, previewHandle any, content string) error {
	h, ok := previewHandle.(*telegramPreviewHandle)
	if !ok {
		return fmt.Errorf("telegram: invalid preview handle type %T", previewHandle)
	}

	html := core.MarkdownToTelegramHTML(content)
	slog.Debug("telegram: UpdateMessage",
		"content_len", len(content), "html_len", len(html),
		"content_prefix", truncateForLog(content, 80),
		"html_prefix", truncateForLog(html, 80))

	edit := tgbotapi.NewEditMessageText(h.chatID, h.messageID, html)
	edit.ParseMode = tgbotapi.ModeHTML

	if _, err := p.bot.Send(edit); err != nil {
		errMsg := err.Error()
		slog.Debug("telegram: UpdateMessage HTML failed", "error", errMsg)
		if strings.Contains(errMsg, "not modified") {
			return nil
		}
		if strings.Contains(errMsg, "can't parse") {
			slog.Debug("telegram: UpdateMessage falling back to plain text", "full_html", html)
			edit.Text = content
			edit.ParseMode = ""
			if _, err2 := p.bot.Send(edit); err2 != nil {
				if strings.Contains(err2.Error(), "not modified") {
					return nil
				}
				return fmt.Errorf("telegram: edit message: %w", err2)
			}
			storeTelegramReplyGroup(h.chatID, []int{h.messageID}, content)
			return nil
		}
		return fmt.Errorf("telegram: edit message: %w", err)
	}
	slog.Debug("telegram: UpdateMessage HTML success")
	storeTelegramReplyGroup(h.chatID, []int{h.messageID}, content)
	return nil
}

func (p *Platform) sendTelegramMessage(chatID int64, content string, markup any, reply bool, replyToMessageID int) (tgbotapi.Message, error) {
	html := core.MarkdownToTelegramHTML(content)
	msg := tgbotapi.NewMessage(chatID, html)
	msg.ParseMode = tgbotapi.ModeHTML
	if reply {
		msg.ReplyToMessageID = replyToMessageID
	}
	if markup != nil {
		msg.ReplyMarkup = markup
	}

	sent, err := p.bot.Send(msg)
	if err != nil {
		if strings.Contains(err.Error(), "can't parse") {
			msg.Text = content
			msg.ParseMode = ""
			sent, err = p.bot.Send(msg)
		}
		if err != nil {
			return tgbotapi.Message{}, fmt.Errorf("telegram: send: %w", err)
		}
	}
	return sent, nil
}

func splitTelegramMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		end := maxLen
		if idx := strings.LastIndex(text[:end], "\n"); idx > 0 && idx >= end/2 {
			end = idx + 1
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks
}

func telegramCommandText(msg *tgbotapi.Message) string {
	if msg == nil {
		return ""
	}
	if strings.TrimSpace(msg.Text) != "" {
		return strings.TrimSpace(msg.Text)
	}
	return strings.TrimSpace(msg.Caption)
}

func telegramCommandName(text string) string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return ""
	}
	cmd := strings.TrimPrefix(fields[0], "/")
	if at := strings.Index(cmd, "@"); at >= 0 {
		cmd = cmd[:at]
	}
	return strings.ToLower(cmd)
}

func extractEntityText(text string, offsetUTF16, lengthUTF16 int) string {
	encoded := utf16.Encode([]rune(text))
	endUTF16 := offsetUTF16 + lengthUTF16
	if offsetUTF16 < 0 || endUTF16 > len(encoded) {
		return ""
	}
	return string(utf16.Decode(encoded[offsetUTF16:endUTF16]))
}

func isTelegramCrossBotCommand(cmd string) bool {
	switch cmd {
	case "sendto", "quote", "st", "sd":
		return true
	default:
		return false
	}
}

func telegramExplicitCommandTarget(text string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", false
	}

	first := strings.TrimPrefix(fields[0], "/")
	if at := strings.Index(first, "@"); at >= 0 && at+1 < len(first) {
		return first[at+1:], true
	}

	if !isTelegramCrossBotCommand(telegramCommandName(text)) || len(fields) < 2 {
		return "", false
	}

	target := strings.TrimSpace(fields[1])
	if strings.HasPrefix(target, "@") && len(target) > 1 {
		return strings.TrimPrefix(target, "@"), true
	}
	return "", false
}

func storeTelegramReplyGroup(chatID int64, messageIDs []int, fullText string) {
	if len(messageIDs) == 0 || strings.TrimSpace(fullText) == "" {
		return
	}
	now := time.Now()
	expiresAt := now.Add(telegramReplyRegistryTTL)

	telegramReplyRegistry.mu.Lock()
	defer telegramReplyRegistry.mu.Unlock()
	cleanupTelegramReplyRegistryLocked(now)
	perChat := telegramReplyRegistry.byChat[chatID]
	if perChat == nil {
		perChat = make(map[int]storedReplyText)
		telegramReplyRegistry.byChat[chatID] = perChat
	}
	for _, messageID := range messageIDs {
		perChat[messageID] = storedReplyText{fullText: fullText, expiresAt: expiresAt}
	}
}

func storeTelegramIncomingReplyReference(msg *tgbotapi.Message) {
	replyToMessageID, replyToContent := telegramDirectReplyReference(msg)
	if replyToMessageID == "" && replyToContent == "" {
		return
	}
	if msg == nil || msg.Chat == nil || msg.MessageID == 0 {
		return
	}

	now := time.Now()
	expiresAt := now.Add(telegramReplyRegistryTTL)

	telegramIncomingReplyRegistry.mu.Lock()
	defer telegramIncomingReplyRegistry.mu.Unlock()
	cleanupTelegramIncomingReplyRegistryLocked(now)
	perChat := telegramIncomingReplyRegistry.byChat[msg.Chat.ID]
	if perChat == nil {
		perChat = make(map[int]storedIncomingReplyReference)
		telegramIncomingReplyRegistry.byChat[msg.Chat.ID] = perChat
	}
	perChat[msg.MessageID] = storedIncomingReplyReference{
		replyToMessageID: replyToMessageID,
		replyToContent:   replyToContent,
		expiresAt:        expiresAt,
	}
}

func resolveTelegramReplyGroup(chatID int64, messageID int) (string, bool) {
	now := time.Now()
	telegramReplyRegistry.mu.Lock()
	defer telegramReplyRegistry.mu.Unlock()
	cleanupTelegramReplyRegistryLocked(now)
	perChat := telegramReplyRegistry.byChat[chatID]
	if perChat == nil {
		return "", false
	}
	entry, ok := perChat[messageID]
	if !ok {
		return "", false
	}
	return entry.fullText, true
}

func resolveTelegramIncomingReplyReference(chatID int64, messageID int) (storedIncomingReplyReference, bool) {
	now := time.Now()
	telegramIncomingReplyRegistry.mu.Lock()
	defer telegramIncomingReplyRegistry.mu.Unlock()
	cleanupTelegramIncomingReplyRegistryLocked(now)
	perChat := telegramIncomingReplyRegistry.byChat[chatID]
	if perChat == nil {
		return storedIncomingReplyReference{}, false
	}
	entry, ok := perChat[messageID]
	if !ok {
		return storedIncomingReplyReference{}, false
	}
	return entry, true
}

func waitForTelegramIncomingReplyReference(chatID int64, messageID int, timeout time.Duration) (storedIncomingReplyReference, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if entry, ok := resolveTelegramIncomingReplyReference(chatID, messageID); ok {
			return entry, true
		}
		if time.Now().After(deadline) {
			return storedIncomingReplyReference{}, false
		}
		time.Sleep(telegramIncomingReplyLookupStep)
	}
}

func cleanupTelegramReplyRegistryLocked(now time.Time) {
	for chatID, perChat := range telegramReplyRegistry.byChat {
		for messageID, entry := range perChat {
			if now.After(entry.expiresAt) {
				delete(perChat, messageID)
			}
		}
		if len(perChat) == 0 {
			delete(telegramReplyRegistry.byChat, chatID)
		}
	}
}

func cleanupTelegramIncomingReplyRegistryLocked(now time.Time) {
	for chatID, perChat := range telegramIncomingReplyRegistry.byChat {
		for messageID, entry := range perChat {
			if now.After(entry.expiresAt) {
				delete(perChat, messageID)
			}
		}
		if len(perChat) == 0 {
			delete(telegramIncomingReplyRegistry.byChat, chatID)
		}
	}
}

func telegramDirectReplyReference(msg *tgbotapi.Message) (string, string) {
	if msg == nil || msg.ReplyToMessage == nil {
		return "", ""
	}
	replyToContent := strings.TrimSpace(msg.ReplyToMessage.Text)
	if replyToContent == "" {
		replyToContent = strings.TrimSpace(msg.ReplyToMessage.Caption)
	}
	return strconv.Itoa(msg.ReplyToMessage.MessageID), replyToContent
}

func telegramReplyReference(msg *tgbotapi.Message) (string, string) {
	replyToMessageID, replyToContent := telegramDirectReplyReference(msg)
	if replyToMessageID != "" || replyToContent != "" {
		return replyToMessageID, replyToContent
	}
	if msg == nil || msg.Chat == nil || msg.MessageID == 0 {
		return "", ""
	}
	if entry, ok := resolveTelegramIncomingReplyReference(msg.Chat.ID, msg.MessageID); ok {
		return entry.replyToMessageID, entry.replyToContent
	}
	if isTelegramCrossBotCommand(telegramCommandName(telegramCommandText(msg))) {
		if entry, ok := waitForTelegramIncomingReplyReference(msg.Chat.ID, msg.MessageID, telegramIncomingReplyLookupTimeout); ok {
			return entry.replyToMessageID, entry.replyToContent
		}
	}
	return "", ""
}

func telegramReplyToMessageID(msg *tgbotapi.Message) string {
	replyToMessageID, _ := telegramReplyReference(msg)
	return replyToMessageID
}

func telegramReplyToContent(msg *tgbotapi.Message) string {
	_, replyToContent := telegramReplyReference(msg)
	return replyToContent
}

func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// StartTyping sends a "typing…" chat action and repeats every 5 seconds
// until the returned stop function is called.
func (p *Platform) StartTyping(ctx context.Context, rctx any) (stop func()) {
	rc, ok := rctx.(replyContext)
	if !ok {
		return func() {}
	}

	action := tgbotapi.NewChatAction(rc.chatID, tgbotapi.ChatTyping)
	p.bot.Send(action)

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.bot.Send(action)
			}
		}
	}()

	return func() { close(done) }
}

func (p *Platform) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.bot != nil {
		p.bot.StopReceivingUpdates()
	}
	return nil
}

// RegisterCommands registers bot commands with Telegram for the command menu.
func (p *Platform) RegisterCommands(commands []core.BotCommandInfo) error {
	if p.bot == nil {
		return fmt.Errorf("telegram: bot not initialized")
	}

	// Telegram limits: max 100 commands, description max 256 chars
	var tgCommands []tgbotapi.BotCommand
	for _, c := range commands {
		if !isValidTelegramCommand(c.Command) {
			slog.Warn("telegram: invalid command, skipping",
				slog.String("command", c.Command),
				slog.String("description", c.Description))
			continue
		}
		desc := c.Description
		if len(desc) > 256 {
			desc = desc[:253] + "..."
		}
		tgCommands = append(tgCommands, tgbotapi.BotCommand{
			Command:     c.Command,
			Description: desc,
		})
	}

	// Limit to 100 commands
	if len(tgCommands) > 100 {
		tgCommands = tgCommands[:100]
	}

	if len(tgCommands) == 0 {
		slog.Debug("telegram: no commands to register")
		return nil
	}

	cfg := tgbotapi.NewSetMyCommands(tgCommands...)
	_, err := p.bot.Request(cfg)
	if err != nil {
		return fmt.Errorf("telegram: setMyCommands failed: %w", err)
	}

	slog.Info("telegram: registered bot commands", "count", len(tgCommands))
	return nil
}

// isValidTelegramCommand validates if a command string meets Telegram's requirements.
// Telegram command rules:
//   - 1-32 characters long
//   - Only lowercase letters, digits, and underscores
//   - Must start with a letter
func isValidTelegramCommand(cmd string) bool {
	if len(cmd) == 0 || len(cmd) > 32 {
		return false
	}
	// Must start with a letter
	if cmd[0] < 'a' || cmd[0] > 'z' {
		return false
	}
	// Rest can be letters, digits, or underscores
	for i := 1; i < len(cmd); i++ {
		c := cmd[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}
