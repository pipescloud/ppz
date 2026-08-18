package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
)

// `ppz pipe set` gets two HTTP endpoints, mirroring the create split:
//
//	PATCH /api/v1/sources/{handle}/pipes/{name}   collared
//	PATCH /api/v1/pipes                           uncollared (body-addressed)
//
// The uncollared one is body-addressed rather than path-addressed for
// the same reason DELETE /api/v1/pipes is: an uncollared pipe's address
// is (manifold, name), and manifolds are dotted paths that don't survive
// a single path segment.

func TestPipeSetAPI_CollaredRoute_NoAuth401(t *testing.T) {
	srv := &Server{}
	body := bytes.NewReader([]byte(`{"max_msgs":5}`))
	req := httptest.NewRequest("PATCH", "/api/v1/sources/chat/pipes/archive", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401 (route must be mounted behind requireAPIKey)", rec.Code)
	}
}

func TestPipeSetAPI_UncollaredRoute_NoAuth401(t *testing.T) {
	srv := &Server{}
	body := bytes.NewReader([]byte(`{"manifold":"","name":"room","max_msgs":5}`))
	req := httptest.NewRequest("PATCH", "/api/v1/pipes", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401 (route must be mounted behind requireAPIKey)", rec.Code)
	}
}

// Symmetry with handleCreatePipeFullPath: the uncollared endpoint
// refuses a collared body instead of silently addressing the wrong pipe.
func TestPipeSetAPI_RejectsCollaredOnUncollaredEndpoint(t *testing.T) {
	srv := &Server{}
	body := []byte(`{"handle":"chat","name":"archive","max_msgs":5}`)
	req := httptest.NewRequest("PATCH", "/api/v1/pipes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleSetPipeFullPath(rec, req, pipeSetTestKey())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (collared request on uncollared endpoint)", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("E_INVALID_PIPE")) {
		t.Errorf("body should carry E_INVALID_PIPE, got %s", rec.Body.String())
	}
}

// A PATCH naming none of the three knobs is a no-op the caller almost
// certainly didn't mean — reject it before touching the DB rather than
// echoing back unchanged retention as if something happened.
func TestPipeSetAPI_NoOverridesIsRejected(t *testing.T) {
	t.Run("uncollared", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest("PATCH", "/api/v1/pipes", bytes.NewReader([]byte(`{"manifold":"","name":"room"}`)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleSetPipeFullPath(rec, req, pipeSetTestKey())
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d want 400 (no retention fields set)", rec.Code)
		}
	})
	t.Run("collared", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest("PATCH", "/api/v1/sources/chat/pipes/archive", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("handle", "chat")
		req.SetPathValue("name", "archive")
		rec := httptest.NewRecorder()
		srv.handleSetPipe(rec, req, pipeSetTestKey())
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d want 400 (no retention fields set)", rec.Code)
		}
	})
}

// An invalid handle is caught before any DB work, same as every other
// collared endpoint.
func TestPipeSetAPI_InvalidHandleRejected(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest("PATCH", "/api/v1/sources/NOPE!/pipes/archive", bytes.NewReader([]byte(`{"max_msgs":5}`)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("handle", "NOPE!")
	req.SetPathValue("name", "archive")
	rec := httptest.NewRecorder()
	srv.handleSetPipe(rec, req, pipeSetTestKey())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (invalid handle)", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("E_INVALID_HANDLE")) {
		t.Errorf("body should carry E_INVALID_HANDLE, got %s", rec.Body.String())
	}
}

// Unlike `pipe create`, `pipe set` MUST accept reserved / auto-pipe
// names: stdout and inbox are exactly the pipes whose 5000-message cap
// users hit first. Validation therefore rejects malformed names but not
// reserved ones — so the guard has to be a plain name-shape check, not
// ValidateUserPipeName.
func TestPipeSetAPI_MalformedNameRejected(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest("PATCH", "/api/v1/pipes", bytes.NewReader([]byte(`{"manifold":"","name":"NOT VALID","max_msgs":5}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleSetPipeFullPath(rec, req, pipeSetTestKey())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (malformed pipe name)", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("E_INVALID_PIPE")) {
		t.Errorf("body should carry E_INVALID_PIPE, got %s", rec.Body.String())
	}
}

// mergeRetention is the merge half of the story the db layer deliberately
// doesn't do: request pointers (nil = "leave alone") applied onto the
// stored row's pointers (nil = "no override, use default"). Getting this
// backwards would make `pipe set --ttl=1h` silently wipe a previously-set
// max-msgs back to the default.
func TestMergeRetention_NilRequestFieldsPreserveStored(t *testing.T) {
	storedTTL, storedMsgs := 3600, 10
	storedBytes := int64(2048)
	stored := db.Pipe{TTLSeconds: &storedTTL, MaxMsgs: &storedMsgs, MaxBytes: &storedBytes}

	newTTL := 7200
	ttl, msgs, byts := mergeRetention(stored, &newTTL, nil, nil)

	if ttl == nil || *ttl != 7200 {
		t.Errorf("ttl = %v, want 7200 (request wins)", deref(ttl))
	}
	if msgs == nil || *msgs != 10 {
		t.Errorf("maxMsgs = %v, want 10 (stored preserved — request was silent)", deref(msgs))
	}
	if byts == nil || *byts != 2048 {
		t.Errorf("maxBytes = %v, want 2048 (stored preserved — request was silent)", deref64(byts))
	}
}

// The TTL arm of the same contract, which the two cases above leave
// open: one sets TTL on the request, the other has no stored TTL to
// preserve. Neither would notice `outTTL := ttl` — a silent request
// clobbering a configured TTL — so a `pipe set --max-bytes=1M` could
// quietly reset a pipe's 1h TTL back to the 24h default.
func TestMergeRetention_SilentRequestPreservesStoredTTL(t *testing.T) {
	storedTTL := 3600
	stored := db.Pipe{TTLSeconds: &storedTTL}

	newBytes := int64(1 << 20)
	ttl, msgs, byts := mergeRetention(stored, nil, nil, &newBytes)

	if ttl == nil || *ttl != 3600 {
		t.Errorf("ttl = %v, want 3600 (stored preserved — request was silent on ttl)", deref(ttl))
	}
	if msgs != nil {
		t.Errorf("maxMsgs = %v, want nil (never set)", deref(msgs))
	}
	if byts == nil || *byts != 1<<20 {
		t.Errorf("maxBytes = %v, want 1048576 (request wins)", deref64(byts))
	}
}

// The capability that distinguishes `pipe set` from `pipe create`: it
// must accept reserved and auto-provisioned names. inbox / stdout are
// precisely the pipes whose default caps users hit first, and being
// auto-provisioned they can never be reached through create — so a
// create-time gate here would make the headline use case impossible
// while every negative-path test above still passed.
func TestValidateSettablePipeName_AcceptsReservedAndAutoNames(t *testing.T) {
	accept := []string{"inbox", "stdout", "stdin", "stdctrl", "system", "heartbeat", "broadcast", "archive"}
	for _, name := range accept {
		if !validateSettablePipeName(name) {
			t.Errorf("validateSettablePipeName(%q) = false, want true — pipe set must be able to retune reserved / auto-provisioned pipes", name)
		}
	}
	reject := []string{"", "NOT VALID", "has.dot", "no/slash", "*"}
	for _, name := range reject {
		if validateSettablePipeName(name) {
			t.Errorf("validateSettablePipeName(%q) = true, want false — name shape is still enforced", name)
		}
	}
}

func TestMergeRetention_UnsetStoredStaysUnset(t *testing.T) {
	msgs := 5
	gotTTL, gotMsgs, gotBytes := mergeRetention(db.Pipe{}, nil, &msgs, nil)
	if gotTTL != nil {
		t.Errorf("ttl = %v, want nil (never set, request silent — must stay on the default layer)", deref(gotTTL))
	}
	if gotMsgs == nil || *gotMsgs != 5 {
		t.Errorf("maxMsgs = %v, want 5", deref(gotMsgs))
	}
	if gotBytes != nil {
		t.Errorf("maxBytes = %v, want nil", deref64(gotBytes))
	}
}

func pipeSetTestKey() db.APIKey {
	return db.APIKey{
		AccountID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		CreatedByUserID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func deref64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
