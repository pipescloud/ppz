package db

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// `ppz pipe set` needs to mutate a pipe row's retention overrides after
// creation. Until now the only writes to ttl_seconds/max_msgs/max_bytes
// were at INSERT time, so the columns were effectively immutable.
//
// UpdatePipeRetention takes the FULL resolved triple, not a partial
// patch: nil means "store NULL here" (i.e. fall back to the default
// layer), not "leave the column alone". Merging a partial request onto
// the stored row is the caller's job — it needs the old row anyway to
// echo the resolved retention back, and keeping the merge out of SQL
// means one place decides precedence.
func TestUpdatePipeRetention_Signature(t *testing.T) {
	fn := reflect.TypeOf(UpdatePipeRetention)
	want := []reflect.Type{
		reflect.TypeOf((*context.Context)(nil)).Elem(),
		reflect.TypeOf((*Pool)(nil)),
		reflect.TypeOf(uuid.UUID{}),
		reflect.TypeOf((*int)(nil)),
		reflect.TypeOf((*int)(nil)),
		reflect.TypeOf((*int64)(nil)),
	}
	if fn.NumIn() != len(want) {
		t.Fatalf("UpdatePipeRetention takes %d args, want %d (ctx, pool, id, ttl, maxMsgs, maxBytes)", fn.NumIn(), len(want))
	}
	for i, w := range want {
		if fn.In(i) != w {
			t.Errorf("UpdatePipeRetention arg %d = %v, want %v", i, fn.In(i), w)
		}
	}
	if fn.NumOut() != 1 || fn.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
		t.Errorf("UpdatePipeRetention must return exactly one error")
	}
}

// The `pipe set` path also has to reach pipes addressed the uncollared
// way (account + manifold + name), which has no source_id to key on.
// GetPipeByName only covers the collared shape.
func TestGetUncollaredPipeByName_Signature(t *testing.T) {
	fn := reflect.TypeOf(GetUncollaredPipeByName)
	want := []reflect.Type{
		reflect.TypeOf((*context.Context)(nil)).Elem(),
		reflect.TypeOf((*Pool)(nil)),
		reflect.TypeOf(uuid.UUID{}),
		reflect.TypeOf(""),
		reflect.TypeOf(""),
	}
	if fn.NumIn() != len(want) {
		t.Fatalf("GetUncollaredPipeByName takes %d args, want %d (ctx, pool, accountID, manifold, name)", fn.NumIn(), len(want))
	}
	for i, w := range want {
		if fn.In(i) != w {
			t.Errorf("GetUncollaredPipeByName arg %d = %v, want %v", i, fn.In(i), w)
		}
	}
	if fn.NumOut() != 2 || fn.Out(0) != reflect.TypeOf(Pipe{}) {
		t.Errorf("GetUncollaredPipeByName must return (Pipe, error)")
	}
}

// Setting retention on an AUTO-provisioned pipe (stdout, inbox, …) has
// nowhere to persist: those pipes are JetStream-only, with no row in the
// `pipes` table. Rather than a parallel overrides table, `pipe set`
// materialises a row on first override — the existing nil-means-default
// resolution then works unchanged, and every reader already de-dupes
// auto-pipe names against the user-pipe list.
//
// The upsert has to be reachable for reserved names, which InsertPipe's
// callers gate on, so it's a distinct entry point rather than a flag.
func TestUpsertPipeRetention_Signature(t *testing.T) {
	fn := reflect.TypeOf(UpsertPipeRetention)
	want := []reflect.Type{
		reflect.TypeOf((*context.Context)(nil)).Elem(),
		reflect.TypeOf((*Pool)(nil)),
		reflect.TypeOf(uuid.UUID{}),       // accountID
		reflect.TypeOf(""),                // manifold
		reflect.TypeOf((*uuid.UUID)(nil)), // sourceID (nil = uncollared)
		reflect.TypeOf(uuid.UUID{}),       // createdBy
		reflect.TypeOf(""),                // name
		reflect.TypeOf((*int)(nil)),
		reflect.TypeOf((*int)(nil)),
		reflect.TypeOf((*int64)(nil)),
	}
	if fn.NumIn() != len(want) {
		t.Fatalf("UpsertPipeRetention takes %d args, want %d", fn.NumIn(), len(want))
	}
	for i, w := range want {
		if fn.In(i) != w {
			t.Errorf("UpsertPipeRetention arg %d = %v, want %v", i, fn.In(i), w)
		}
	}
	if fn.NumOut() != 2 || fn.Out(0) != reflect.TypeOf(Pipe{}) {
		t.Errorf("UpsertPipeRetention must return (Pipe, error)")
	}
}
