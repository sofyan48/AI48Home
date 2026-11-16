package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// UserChatConversationLimit caps stored conversations per user; the oldest are pruned.
	UserChatConversationLimit = 100
	// UserChatMessageLimit caps the number of messages stored in one conversation.
	UserChatMessageLimit = 500
	// UserChatIDMaxLength bounds client-generated conversation identifiers.
	UserChatIDMaxLength    = 64
	userChatTitleMaxLength = 200
	userChatModelMaxLength = 256
)

// ErrInvalidUserChat reports a malformed conversation payload.
var ErrInvalidUserChat = errors.New("chat conversation is invalid")

// UserChatConversationRecord stores one playground conversation owned by a user.
// The client generates the conversation ID; ownership is part of the primary key.
type UserChatConversationRecord struct {
	UserID    uint      `gorm:"column:user_id;primaryKey;autoIncrement:false;index:idx_user_chat_user_updated,priority:1"`
	ID        string    `gorm:"column:id;primaryKey;size:64"`
	Title     string    `gorm:"column:title;not null;default:''"`
	Model     string    `gorm:"column:model;not null;default:''"`
	Messages  JSONB     `gorm:"column:messages;not null"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;index:idx_user_chat_user_updated,priority:2,sort:desc"`
}

func (UserChatConversationRecord) TableName() string { return "user_chat_conversation" }

// UserChatUsage mirrors the OpenAI-compatible usage object kept for display.
type UserChatUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens *int64 `json:"completion_tokens,omitempty"`
	TotalTokens      *int64 `json:"total_tokens,omitempty"`
}

// UserChatMessage is one stored chat turn.
type UserChatMessage struct {
	ID        string         `json:"id"`
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Reasoning string         `json:"reasoning,omitempty"`
	Model     string         `json:"model,omitempty"`
	Stopped   bool           `json:"stopped,omitempty"`
	Error     string         `json:"error,omitempty"`
	Usage     *UserChatUsage `json:"usage,omitempty"`
	LatencyMS *int64         `json:"latency_ms,omitempty"`
}

// UserChatConversation is the decoded form of a stored conversation.
type UserChatConversation struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	Model        string            `json:"model"`
	MessageCount int               `json:"message_count"`
	Messages     []UserChatMessage `json:"messages,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// UserChatConversationInput is the client payload for saving a conversation.
type UserChatConversationInput struct {
	Title    string            `json:"title"`
	Model    string            `json:"model"`
	Messages []UserChatMessage `json:"messages"`
}

// ValidUserChatID reports whether a client-generated conversation ID is acceptable.
func ValidUserChatID(id string) bool {
	if id == "" || len(id) > UserChatIDMaxLength {
		return false
	}
	for _, char := range id {
		isAlnum := (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')
		if !isAlnum && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func truncateRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}

func normalizeUserChatInput(input UserChatConversationInput) (UserChatConversationInput, error) {
	if len(input.Messages) == 0 {
		return input, fmt.Errorf("%w: messages are required", ErrInvalidUserChat)
	}
	if len(input.Messages) > UserChatMessageLimit {
		return input, fmt.Errorf("%w: at most %d messages are allowed", ErrInvalidUserChat, UserChatMessageLimit)
	}
	messages := make([]UserChatMessage, 0, len(input.Messages))
	for index, message := range input.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			return input, fmt.Errorf("%w: messages[%d].role must be user or assistant", ErrInvalidUserChat, index)
		}
		message.ID = truncateRunes(message.ID, UserChatIDMaxLength)
		message.Model = truncateRunes(message.Model, userChatModelMaxLength)
		messages = append(messages, message)
	}
	input.Messages = messages
	input.Model = truncateRunes(input.Model, userChatModelMaxLength)
	input.Title = truncateRunes(input.Title, userChatTitleMaxLength)
	if input.Title == "" {
		for _, message := range messages {
			if message.Role == "user" && strings.TrimSpace(message.Content) != "" {
				input.Title = truncateRunes(strings.Join(strings.Fields(message.Content), " "), 60)
				break
			}
		}
	}
	return input, nil
}

func userChatConversationFromRecord(record UserChatConversationRecord, withMessages bool) (UserChatConversation, error) {
	var messages []UserChatMessage
	if len(record.Messages) > 0 {
		if errUnmarshal := json.Unmarshal(record.Messages, &messages); errUnmarshal != nil {
			return UserChatConversation{}, fmt.Errorf("decode chat conversation messages: %w", errUnmarshal)
		}
	}
	conversation := UserChatConversation{
		ID: record.ID, Title: record.Title, Model: record.Model, MessageCount: len(messages),
		CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
	if withMessages {
		if messages == nil {
			messages = []UserChatMessage{}
		}
		conversation.Messages = messages
	}
	return conversation, nil
}

// ListUserChatConversations lists a user's conversations, newest first, without messages.
func (r *Repository) ListUserChatConversations(ctx context.Context, userID uint) ([]UserChatConversation, error) {
	if userID == 0 {
		return nil, fmt.Errorf("user id is required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var records []UserChatConversationRecord
	errFind := db.WithContext(contextOrBackground(ctx)).
		Where("user_id = ?", userID).
		Order("updated_at DESC").Order("id ASC").
		Limit(UserChatConversationLimit).
		Find(&records).Error
	if errFind != nil {
		return nil, errFind
	}
	items := make([]UserChatConversation, 0, len(records))
	for _, record := range records {
		item, errDecode := userChatConversationFromRecord(record, false)
		if errDecode != nil {
			return nil, errDecode
		}
		items = append(items, item)
	}
	return items, nil
}

// GetUserChatConversation loads one conversation owned by the user.
func (r *Repository) GetUserChatConversation(ctx context.Context, userID uint, id string) (*UserChatConversation, error) {
	if userID == 0 {
		return nil, fmt.Errorf("user id is required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var record UserChatConversationRecord
	if errFirst := db.WithContext(contextOrBackground(ctx)).Where("user_id = ? AND id = ?", userID, id).First(&record).Error; errFirst != nil {
		return nil, errFirst
	}
	conversation, errDecode := userChatConversationFromRecord(record, true)
	if errDecode != nil {
		return nil, errDecode
	}
	return &conversation, nil
}

// SaveUserChatConversation creates or replaces a conversation and prunes the oldest beyond the limit.
func (r *Repository) SaveUserChatConversation(ctx context.Context, userID uint, id string, input UserChatConversationInput) (*UserChatConversation, error) {
	if userID == 0 {
		return nil, fmt.Errorf("user id is required")
	}
	if !ValidUserChatID(id) {
		return nil, fmt.Errorf("%w: id is invalid", ErrInvalidUserChat)
	}
	normalized, errInput := normalizeUserChatInput(input)
	if errInput != nil {
		return nil, errInput
	}
	encoded, errEncode := json.Marshal(normalized.Messages)
	if errEncode != nil {
		return nil, fmt.Errorf("encode chat conversation messages: %w", errEncode)
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	now := time.Now().UTC()
	record := UserChatConversationRecord{
		UserID: userID, ID: id, Title: normalized.Title, Model: normalized.Model,
		Messages: JSONB(encoded), CreatedAt: now, UpdatedAt: now,
	}
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		errUpsert := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}, {Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{"title", "model", "messages", "updated_at"}),
		}).Create(&record).Error
		if errUpsert != nil {
			return errUpsert
		}
		return pruneUserChatConversationsTx(tx, userID)
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return r.GetUserChatConversation(ctx, userID, id)
}

func pruneUserChatConversationsTx(tx *gorm.DB, userID uint) error {
	var stale []string
	errStale := tx.Model(&UserChatConversationRecord{}).
		Where("user_id = ?", userID).
		Order("updated_at DESC").Order("id ASC").
		Offset(UserChatConversationLimit).Limit(1000).
		Pluck("id", &stale).Error
	if errStale != nil || len(stale) == 0 {
		return errStale
	}
	return tx.Where("user_id = ? AND id IN ?", userID, stale).Delete(&UserChatConversationRecord{}).Error
}

// DeleteUserChatConversation deletes one conversation owned by the user.
func (r *Repository) DeleteUserChatConversation(ctx context.Context, userID uint, id string) error {
	if userID == 0 {
		return fmt.Errorf("user id is required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	result := db.WithContext(contextOrBackground(ctx)).Where("user_id = ? AND id = ?", userID, id).Delete(&UserChatConversationRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// DeleteAllUserChatConversations deletes every conversation owned by the user.
func (r *Repository) DeleteAllUserChatConversations(ctx context.Context, userID uint) error {
	if userID == 0 {
		return fmt.Errorf("user id is required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	return db.WithContext(contextOrBackground(ctx)).Where("user_id = ?", userID).Delete(&UserChatConversationRecord{}).Error
}
