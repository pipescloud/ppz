package db

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Pipe mutations become auditable. The table is deliberately GENERIC
// (an `audit_events` row describes any actor/action/target, with
// before+after payloads as jsonb) rather than pipe-retention-specific:
// pipe create/set/destroy are simply its first three writers, and key
// revoke / member removal / source destroy are the obvious next ones.
// A retention-shaped table would have to be thrown away to get there.

func TestAuditEvent_Fields(t *testing.T) {
	rt := reflect.TypeOf(AuditEvent{})
	for field, want := range map[string]reflect.Type{
		"ID":            reflect.TypeOf(uuid.UUID{}),
		"AccountID":     reflect.TypeOf(uuid.UUID{}),
		"ActorUserID":   reflect.TypeOf(uuid.UUID{}),
		"ActorAPIKeyID": reflect.TypeOf((*uuid.UUID)(nil)),
		"Action":        reflect.TypeOf(""),
		"TargetType":    reflect.TypeOf(""),
		"Target":        reflect.TypeOf(""),
		"Before":        reflect.TypeOf([]byte(nil)),
		"After":         reflect.TypeOf([]byte(nil)),
		"CreatedAt":     reflect.TypeOf(time.Time{}),
	} {
		f, ok := rt.FieldByName(field)
		if !ok {
			t.Errorf("AuditEvent.%s missing", field)
			continue
		}
		if f.Type != want {
			t.Errorf("AuditEvent.%s type = %v, want %v", field, f.Type, want)
		}
	}
}

// ActorAPIKeyID is a POINTER on purpose, and this is the field that
// keeps the trail honest. The API-key path can only name the key's
// CREATOR, not whoever typed the command — with a shared org key every
// `ppz pipe set` attributes to whoever minted it. Recording which key
// was used lets the GUI say "via key ppz_ab12…" instead of implying a
// person was at a keyboard. A web-session actor leaves it nil.
func TestAuditEvent_ActorAPIKeyIDIsNillable(t *testing.T) {
	f, ok := reflect.TypeOf(AuditEvent{}).FieldByName("ActorAPIKeyID")
	if !ok {
		t.Fatal("AuditEvent.ActorAPIKeyID missing")
	}
	if f.Type.Kind() != reflect.Ptr {
		t.Errorf("ActorAPIKeyID must be a pointer (nil = acted via web session, not an API key), got %v", f.Type)
	}
}

func TestInsertAuditEvent_Signature(t *testing.T) {
	fn := reflect.TypeOf(InsertAuditEvent)
	want := []reflect.Type{
		reflect.TypeOf((*context.Context)(nil)).Elem(),
		reflect.TypeOf((*Pool)(nil)),
		reflect.TypeOf(AuditEvent{}),
	}
	if fn.NumIn() != len(want) {
		t.Fatalf("InsertAuditEvent takes %d args, want %d (ctx, pool, event)", fn.NumIn(), len(want))
	}
	for i, w := range want {
		if fn.In(i) != w {
			t.Errorf("InsertAuditEvent arg %d = %v, want %v", i, fn.In(i), w)
		}
	}
	if fn.NumOut() != 1 || fn.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
		t.Errorf("InsertAuditEvent must return exactly one error")
	}
}

// The GUI tab reads newest-first with a bound, so a long-lived org
// doesn't render ten thousand rows.
func TestListAuditEventsForAccount_Signature(t *testing.T) {
	fn := reflect.TypeOf(ListAuditEventsForAccount)
	want := []reflect.Type{
		reflect.TypeOf((*context.Context)(nil)).Elem(),
		reflect.TypeOf((*Pool)(nil)),
		reflect.TypeOf(uuid.UUID{}),
		reflect.TypeOf(0), // limit
	}
	if fn.NumIn() != len(want) {
		t.Fatalf("ListAuditEventsForAccount takes %d args, want %d (ctx, pool, accountID, limit)", fn.NumIn(), len(want))
	}
	for i, w := range want {
		if fn.In(i) != w {
			t.Errorf("ListAuditEventsForAccount arg %d = %v, want %v", i, fn.In(i), w)
		}
	}
	if fn.NumOut() != 2 || fn.Out(0) != reflect.TypeOf([]AuditEvent(nil)) {
		t.Errorf("ListAuditEventsForAccount must return ([]AuditEvent, error)")
	}
}

// Action strings are a stable contract: they're persisted in every row
// and the GUI filters/labels on them. Dotted <noun>.<verb> so future
// writers (key.revoke, member.remove) read consistently.
func TestAuditActions_PipeLifecycle(t *testing.T) {
	for got, want := range map[string]string{
		AuditActionPipeCreate:  "pipe.create",
		AuditActionPipeSet:     "pipe.set",
		AuditActionPipeDestroy: "pipe.destroy",
	} {
		if got != want {
			t.Errorf("action constant = %q, want %q", got, want)
		}
	}
	if AuditTargetPipe != "pipe" {
		t.Errorf("AuditTargetPipe = %q, want %q", AuditTargetPipe, "pipe")
	}
}
