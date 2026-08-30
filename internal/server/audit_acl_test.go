package server

import (
	"testing"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
)

// auditActorFromKey attributes an API-key action to the key's CREATOR,
// on the reasoning that the server cannot know who typed the command so
// a shared key names whoever minted it. That is right for a shared human
// key — and wrong for a service-account key, where nobody typed anything
// and the bot genuinely IS the actor.
//
// Attributing the bot's work to the human who minted its key makes the
// trail misleading exactly where it matters most: the reader sees a
// person taking an action they never took.

func TestAuditActor_ServiceKeyAttributesToTheService(t *testing.T) {
	human := uuid.New()
	service := uuid.New()
	key := db.APIKey{
		ID:              uuid.New(),
		AccountID:       uuid.New(),
		CreatedByUserID: human,
		PrincipalUserID: service,
	}

	actor, keyID := auditActorFromKey(key)

	if actor == human {
		t.Error("a service-account key attributed its action to the human who minted it")
	}
	if actor != service {
		t.Errorf("actor = %v, want the service principal %v", actor, service)
	}
	if keyID == nil || *keyID != key.ID {
		t.Error("the key id must still travel, so the GUI can render 'via key'")
	}
}

// The ordinary case is unchanged: a human's own key attributes to them,
// because principal and creator are the same identity.
func TestAuditActor_OrdinaryKeyStillAttributesToItsOwner(t *testing.T) {
	human := uuid.New()
	key := db.APIKey{ID: uuid.New(), CreatedByUserID: human, PrincipalUserID: human}

	actor, _ := auditActorFromKey(key)

	if actor != human {
		t.Errorf("actor = %v, want %v", actor, human)
	}
}

// A synthetic key built by the OAuth path carries only CreatedByUserID,
// so the fallback has to hold or session-authed actions lose their actor.
func TestAuditActor_FallsBackWhenPrincipalUnset(t *testing.T) {
	human := uuid.New()
	key := db.APIKey{ID: uuid.New(), CreatedByUserID: human}

	if actor, _ := auditActorFromKey(key); actor != human {
		t.Errorf("actor = %v, want the creator %v as fallback", actor, human)
	}
}
