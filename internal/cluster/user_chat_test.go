package cluster

import (
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"
)

func chatInput(text string) UserChatConversationInput {
	return UserChatConversationInput{
		Model: "gpt-test",
		Messages: []UserChatMessage{
			{ID: "m1", Role: "user", Content: text},
			{ID: "m2", Role: "assistant", Content: "reply", Model: "gpt-test"},
		},
	}
}

func TestUserChatConversationLifecycleIsOwnerScoped(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	alice := createPeriodLimitUser(t, repo, ctx, "chat-alice", 0)
	bob := createPeriodLimitUser(t, repo, ctx, "chat-bob", 0)

	saved, errSave := repo.SaveUserChatConversation(ctx, alice.ID, "chat-1", chatInput("  Hello\n  world  "))
	if errSave != nil {
		t.Fatal(errSave)
	}
	if saved.Title != "Hello world" || saved.MessageCount != 2 || len(saved.Messages) != 2 {
		t.Fatalf("saved = %+v", saved)
	}

	// Saving again replaces messages but keeps the creation time.
	next := chatInput("Hello world")
	next.Messages = append(next.Messages, UserChatMessage{ID: "m3", Role: "user", Content: "again"})
	updated, errUpdate := repo.SaveUserChatConversation(ctx, alice.ID, "chat-1", next)
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if updated.MessageCount != 3 || !updated.CreatedAt.Equal(saved.CreatedAt) {
		t.Fatalf("updated = %+v, created %v", updated, saved.CreatedAt)
	}

	// Another user cannot read or delete it, and may reuse the same client ID.
	if _, errGet := repo.GetUserChatConversation(ctx, bob.ID, "chat-1"); !errors.Is(errGet, gorm.ErrRecordNotFound) {
		t.Fatalf("bob get error = %v", errGet)
	}
	if errDelete := repo.DeleteUserChatConversation(ctx, bob.ID, "chat-1"); !errors.Is(errDelete, gorm.ErrRecordNotFound) {
		t.Fatalf("bob delete error = %v", errDelete)
	}
	if _, errBob := repo.SaveUserChatConversation(ctx, bob.ID, "chat-1", chatInput("bob")); errBob != nil {
		t.Fatal(errBob)
	}

	list, errList := repo.ListUserChatConversations(ctx, alice.ID)
	if errList != nil || len(list) != 1 || list[0].Messages != nil || list[0].MessageCount != 3 {
		t.Fatalf("list = %+v err=%v", list, errList)
	}

	if errDelete := repo.DeleteUserChatConversation(ctx, alice.ID, "chat-1"); errDelete != nil {
		t.Fatal(errDelete)
	}
	if bobList, _ := repo.ListUserChatConversations(ctx, bob.ID); len(bobList) != 1 {
		t.Fatalf("bob conversations = %d, want 1", len(bobList))
	}

	// Deleting the account removes its conversations.
	if errDeleteUser := repo.DeleteUser(ctx, bob.ID); errDeleteUser != nil {
		t.Fatal(errDeleteUser)
	}
	var remaining int64
	if errCount := repo.db.Model(&UserChatConversationRecord{}).Where("user_id = ?", bob.ID).Count(&remaining).Error; errCount != nil || remaining != 0 {
		t.Fatalf("remaining = %d err=%v", remaining, errCount)
	}
}

func TestUserChatConversationValidationAndPruning(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	user := createPeriodLimitUser(t, repo, ctx, "chat-prune", 0)

	invalid := []struct {
		id    string
		input UserChatConversationInput
	}{
		{"bad id!", chatInput("x")},
		{"", chatInput("x")},
		{"ok", UserChatConversationInput{}},
		{"ok", UserChatConversationInput{Messages: []UserChatMessage{{Role: "system", Content: "x"}}}},
	}
	for _, item := range invalid {
		if _, errSave := repo.SaveUserChatConversation(ctx, user.ID, item.id, item.input); !errors.Is(errSave, ErrInvalidUserChat) {
			t.Fatalf("save %q error = %v, want ErrInvalidUserChat", item.id, errSave)
		}
	}

	for index := 0; index < UserChatConversationLimit+3; index++ {
		if _, errSave := repo.SaveUserChatConversation(ctx, user.ID, fmt.Sprintf("chat-%03d", index), chatInput("x")); errSave != nil {
			t.Fatal(errSave)
		}
	}
	var count int64
	if errCount := repo.db.Model(&UserChatConversationRecord{}).Where("user_id = ?", user.ID).Count(&count).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if count != UserChatConversationLimit {
		t.Fatalf("stored conversations = %d, want %d", count, UserChatConversationLimit)
	}

	if errClear := repo.DeleteAllUserChatConversations(ctx, user.ID); errClear != nil {
		t.Fatal(errClear)
	}
	if list, _ := repo.ListUserChatConversations(ctx, user.ID); len(list) != 0 {
		t.Fatalf("after clear = %d", len(list))
	}
}
