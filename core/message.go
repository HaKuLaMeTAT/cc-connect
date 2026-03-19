package core

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxSavedAttachmentSize = 50 << 20 // 50 MB

// MergeEnv returns base env with entries from extra overriding same-key entries.
// This prevents duplicate keys (e.g. two PATH entries) which cause the override
// to be silently ignored on Linux (getenv returns the first match).
func MergeEnv(base, extra []string) []string {
	keys := make(map[string]bool, len(extra))
	for _, e := range extra {
		if k, _, ok := strings.Cut(e, "="); ok {
			keys[k] = true
		}
	}
	merged := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		if k, _, ok := strings.Cut(e, "="); ok && keys[k] {
			continue
		}
		merged = append(merged, e)
	}
	return append(merged, extra...)
}

// AllowList checks whether a user ID is permitted based on a comma-separated
// allow_from string. Returns true if allowFrom is empty or "*" (allow all),
// or if the userID is in the list. Comparison is case-insensitive.
func AllowList(allowFrom, userID string) bool {
	allowFrom = strings.TrimSpace(allowFrom)
	if allowFrom == "" || allowFrom == "*" {
		return true
	}
	for _, id := range strings.Split(allowFrom, ",") {
		if strings.EqualFold(strings.TrimSpace(id), userID) {
			return true
		}
	}
	return false
}

// ImageAttachment represents an image sent by the user.
type ImageAttachment struct {
	MimeType string // e.g. "image/png", "image/jpeg"
	Data     []byte // raw image bytes
	FileName string // original filename (optional)
}

// FileAttachment represents a generic file sent by the user.
type FileAttachment struct {
	MimeType string // e.g. "application/pdf", "text/plain"
	Data     []byte // raw file bytes
	FileName string // original filename (optional)
}

// SaveFilesToDisk persists file attachments under workDir/.cc-connect/attachments
// and returns the absolute paths for prompt injection.
func SaveFilesToDisk(workDir string, files []FileAttachment) []string {
	if len(files) == 0 {
		return nil
	}

	attachDir := filepath.Join(workDir, ".cc-connect", "attachments")
	if err := os.MkdirAll(attachDir, 0o755); err != nil {
		slog.Error("SaveFilesToDisk: mkdir failed", "error", err, "dir", attachDir)
		return nil
	}

	paths := make([]string, 0, len(files))
	for i, f := range files {
		if len(f.Data) > maxSavedAttachmentSize {
			slog.Warn("SaveFilesToDisk: attachment exceeds size limit", "size", len(f.Data), "name", f.FileName)
			continue
		}
		name := sanitizeAttachmentFileName(f.FileName, i)
		fullPath := filepath.Join(attachDir, name)
		if err := os.WriteFile(fullPath, f.Data, 0o644); err != nil {
			slog.Error("SaveFilesToDisk: write failed", "error", err, "path", fullPath)
			continue
		}
		paths = append(paths, fullPath)
	}
	return paths
}

func sanitizeAttachmentFileName(name string, index int) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Sprintf("file_%d_%d", time.Now().UnixMilli(), index)
	}
	name = filepath.Base(filepath.Clean(name))
	switch name {
	case "", ".", string(filepath.Separator), "..":
		return fmt.Sprintf("file_%d_%d", time.Now().UnixMilli(), index)
	default:
		return name
	}
}

// AppendFileRefs appends locally saved file paths to the prompt so agents can read them.
func AppendFileRefs(prompt string, filePaths []string) string {
	if len(filePaths) == 0 {
		return prompt
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "Please analyze the attached file(s)."
	}
	return prompt + "\n\n(Files saved locally, please read them: " + strings.Join(filePaths, ", ") + ")"
}

// AudioAttachment represents a voice/audio message sent by the user.
type AudioAttachment struct {
	MimeType string // e.g. "audio/amr", "audio/ogg", "audio/mp4"
	Data     []byte // raw audio bytes
	Format   string // short format hint: "amr", "ogg", "m4a", "mp3", "wav", etc.
	Duration int    // duration in seconds (if known)
}

// Message represents a unified incoming message from any platform.
type Message struct {
	SessionKey       string // unique key for user context, e.g. "feishu:{chatID}:{userID}"
	Platform         string
	MessageID        string // platform message ID for tracing
	UserID           string
	UserName         string
	Content          string
	ReplyToMessageID string            // platform message id this message replies to (if any)
	ReplyToContent   string            // raw replied message content if the platform exposes it
	Images           []ImageAttachment // attached images (if any)
	Files            []FileAttachment  // attached files (if any)
	Audio            *AudioAttachment  // voice message (if any)
	ReplyCtx         any               // platform-specific context needed for replying
	FromVoice        bool              // true if message originated from voice transcription
}

// EventType distinguishes different kinds of agent output.
type EventType string

const (
	EventText              EventType = "text"               // intermediate or final text
	EventToolUse           EventType = "tool_use"           // tool invocation info
	EventToolResult        EventType = "tool_result"        // tool execution result
	EventResult            EventType = "result"             // final aggregated result
	EventError             EventType = "error"              // error occurred
	EventPermissionRequest EventType = "permission_request" // agent requests permission via stdio protocol
	EventThinking          EventType = "thinking"           // thinking/processing status
)

// Event represents a single piece of agent output streamed back to the engine.
type Event struct {
	Type         EventType
	Content      string
	ToolName     string         // populated for EventToolUse, EventPermissionRequest
	ToolInput    string         // human-readable summary of tool input
	ToolInputRaw map[string]any // raw tool input (for EventPermissionRequest, used in allow response)
	ToolResult   string         // populated for EventToolResult
	SessionID    string         // agent-managed session ID for conversation continuity
	RequestID    string         // unique request ID for EventPermissionRequest
	Done         bool
	Error        error
}

// HistoryEntry is one turn in a conversation.
type HistoryEntry struct {
	Role      string    `json:"role"` // "user" or "assistant"
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// AgentSessionInfo describes one session as reported by the agent backend.
type AgentSessionInfo struct {
	ID           string
	Summary      string
	MessageCount int
	ModifiedAt   time.Time
	GitBranch    string
}
