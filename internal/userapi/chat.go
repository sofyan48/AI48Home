package userapi

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

// maxUserChatBodyBytes allows long conversations while still bounding a single save.
const maxUserChatBodyBytes int64 = 4 << 20

// ListChatConversations lists the authenticated user's playground conversations without messages.
func (h *Handler) ListChatConversations(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	items, errList := h.repo.ListUserChatConversations(ctx, user.ID)
	if errList != nil {
		respondError(c, http.StatusInternalServerError, "chat_load_failed", errList)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "limit": cluster.UserChatConversationLimit})
}

// GetChatConversation returns one conversation with its messages.
func (h *Handler) GetChatConversation(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	id, ok := chatConversationID(c)
	if !ok {
		return
	}
	conversation, errGet := h.repo.GetUserChatConversation(ctx, user.ID, id)
	if errGet != nil {
		respondChatError(c, "chat_load_failed", errGet)
		return
	}
	c.JSON(http.StatusOK, gin.H{"conversation": conversation})
}

// SaveChatConversation creates or replaces a conversation identified by a client-generated ID.
func (h *Handler) SaveChatConversation(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	id, ok := chatConversationID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUserChatBodyBytes)
	var body cluster.UserChatConversationInput
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(errBind, &maxBytesError) {
			respondError(c, http.StatusRequestEntityTooLarge, "request_too_large", errors.New("conversation is too large"))
			return
		}
		respondError(c, http.StatusBadRequest, "invalid_body", errBind)
		return
	}
	conversation, errSave := h.repo.SaveUserChatConversation(ctx, user.ID, id, body)
	if errSave != nil {
		respondChatError(c, "chat_save_failed", errSave)
		return
	}
	c.JSON(http.StatusOK, gin.H{"conversation": conversation})
}

// DeleteChatConversation deletes one conversation.
func (h *Handler) DeleteChatConversation(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	id, ok := chatConversationID(c)
	if !ok {
		return
	}
	if errDelete := h.repo.DeleteUserChatConversation(ctx, user.ID, id); errDelete != nil {
		respondChatError(c, "chat_delete_failed", errDelete)
		return
	}
	respondOK(c)
}

// DeleteAllChatConversations deletes every conversation of the authenticated user.
func (h *Handler) DeleteAllChatConversations(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	if errDelete := h.repo.DeleteAllUserChatConversations(ctx, user.ID); errDelete != nil {
		respondError(c, http.StatusInternalServerError, "chat_delete_failed", errDelete)
		return
	}
	respondOK(c)
}

func chatConversationID(c *gin.Context) (string, bool) {
	id := c.Param("id")
	if !cluster.ValidUserChatID(id) {
		respondError(c, http.StatusBadRequest, "invalid_chat_id", errors.New("conversation id is invalid"))
		return "", false
	}
	return id, true
}

func respondChatError(c *gin.Context, code string, err error) {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		respondError(c, http.StatusNotFound, "not_found", errors.New("conversation not found"))
	case errors.Is(err, cluster.ErrInvalidUserChat):
		respondError(c, http.StatusBadRequest, "invalid_chat", err)
	default:
		respondError(c, http.StatusInternalServerError, code, err)
	}
}
