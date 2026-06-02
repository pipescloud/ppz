package cliproto

import "time"

// IPC method names. Keep in sync with WIRE.md §7.
const (
	IPCStatus      = "Status"
	IPCLogin       = "Login"
	IPCCreate      = "Create"
	IPCSwitch      = "Switch"
	// IPCSend / IPCSendBatch — the publish-IPC verbs used by
	// `ppz send`, `ppz command`, terminal stdin forwarding, etc.
	// Renamed from IPCBroadcast/IPCBroadcastBatch in Phase 1.5 to
	// match the surviving user-facing verb (`ppz broadcast` itself
	// was removed in Phase 1 — locked decision #16).
	IPCSend      = "Send"
	IPCSendBatch = "SendBatch"

	IPCList        = "List"
	IPCListWatch   = "ListWatch"
	IPCSubscribe   = "Subscribe"
	IPCRead        = "Read"
	IPCConnect     = "Connect"
	IPCDisconnect  = "Disconnect"
	IPCPipeCreate    = "PipeCreate"
	IPCPipeDestroy   = "PipeDestroy"
	IPCSourceDestroy = "SourceDestroy"

	// Phase 1.5 namespace (manifold) state verbs. `ppz set namespace
	// PATH` / `ppz unset namespace` — symmetric with handle. No
	// IPCGetNamespace: `ppz status` carries it in StatusReply.
	IPCSetNamespace   = "SetNamespace"
	IPCUnsetNamespace = "UnsetNamespace"

	// Diag verb (Phase 0 — agent hardening). Returns the daemon's
	// recent NATS connection-state events for `ppz diagnostics`. Works
	// without credentials and without a live NATS connection — the
	// whole point is being able to introspect a sick daemon.
	IPCDiag = "Diag"

	// Who verb — `ppz who`. Returns the daemon's in-memory snapshot of
	// the most recent heartbeat per source handle. Client-side filters
	// and rendering happen in cmdWho; the daemon just dumps the cache.
	IPCWho = "Who"
)

// Source kinds, mirrored from internal/db so non-db callers can use them.
type SourceKind string

const (
	KindMessage SourceKind = "message"
	KindPTY     SourceKind = "pty"
)

// ReadRequest carries the parsed `ppz read` parameters from the CLI to the
// daemon. The daemon streams ReadEvent JSON lines back on the same
// connection; the CLI reads until EOF (or until SIGINT closes the socket).
//
// The `channel` JSON tag is preserved for IPC backward-compat within the
// Phase A rename — it carries a pipe name (sub-bucket). Phase B reorganises
// IPC field names alongside the verb refactor.
type ReadRequest struct {
	Handle    string `json:"handle"`
	Channel   string `json:"channel"`            // pipe name: broadcast / stdin / stdout
	Limit     int    `json:"limit,omitempty"`    // 0 = unlimited; non-zero = tail-N (reread only)
	Skip      int    `json:"skip,omitempty"`     // drop the first N retained messages (reread only)
	SinceMS   int64  `json:"since_ms,omitempty"` // 0 = no time filter; >0 = only msgs newer than (now − this many ms) (reread only)
	JSON      bool   `json:"json,omitempty"`     // emit envelope as JSON instead of payload text
	Follow    bool   `json:"follow,omitempty"`
	Session   string `json:"session,omitempty"`    // cursor key — defaults to "default" daemon-side
	NoAdvance bool   `json:"no_advance,omitempty"` // observational reads (terminal view) skip cursor advance
	All       bool   `json:"all,omitempty"`        // forensic mode (`reread`): ignore the cursor (deliver everything) and don't advance it. Implies NoAdvance.

	// Sender is the CLI's resolved current-handle hint for ack:read
	// emission. Mirrors SendRequest.Sender's role: inside a `ppz
	// terminal share` wrapped shell, terminalShareEnv exports
	// PPZ_CURRENT_HANDLE=<handle> but the daemon's IPCCreate skips
	// SetCurrent for PTY-kind sources, so State.Current(req.Session)
	// is empty even though the env says we're the wrapped handle.
	// The daemon's read-path ack auto-emitter (emitAcks at
	// read.go:316,371) stamps envelope.sender from `self` = the
	// reader's own handle; without the hint, acks ship with
	// sender="" and the original sender can't tell who acknowledged.
	// senderForRequest routes the precedence: hint wins, daemon
	// state is the fallback.
	Sender string `json:"sender,omitempty"`

	// Phase 1.5: BareTarget carries the raw user input when `ppz
	// read/reread LEAF` was bare. The daemon resolves it as an
	// uncollared pipe at the session's current_namespace. Handle and
	// Channel are empty in this case.
	BareTarget string `json:"bare_target,omitempty"`
}

// ReadEvent is the wire format of one streamed line in a Read response.
// Exactly one of Message / Error / Meta is set on each event. End-of-stream
// is signaled by the daemon closing the connection.
type ReadEvent struct {
	Message *ReadMessage `json:"msg,omitempty"`
	Error   *Error       `json:"error,omitempty"`
	// Meta is an optional leading event (sent before any Message events)
	// carrying out-of-band stream metadata — currently the source pty's
	// dimensions for `<h>.stdout` reads, sourced from the latest
	// `<h>.stdctrl` resize. Lets the CLI configure its --tty renderer
	// to match the source size before consuming bytes.
	Meta *ReadMeta `json:"meta,omitempty"`
}

// ReadMeta carries leading metadata about the stream. Currently a
// dimension hint for terminal renders; future fields can join here
// (cwd, exit code, last activity ts, etc.) without breaking older
// CLI builds (unknown JSON fields are ignored).
type ReadMeta struct {
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
}

// ReadMessage is the daemon's serialized form of one envelope. The CLI
// formats this as either bare payload text or a JSON object depending on
// whether --json was passed.
//
// Sender mirrors the envelope's `sender` (publisher's current source at
// publish time). Subject mirrors the envelope's `subject` (optional
// header-line, free-form for users / `ack:*` for system messages).
// Both are empty for legacy retained messages published before v0.23.0
// (those carried `handle` instead, which is now silently dropped on
// parse).
type ReadMessage struct {
	ID           string `json:"id"`
	Sender       string `json:"sender"`
	Subject      string `json:"subject"`
	Payload      string `json:"payload"`
	CreatedAt    string `json:"created_at"`
	InReplyTo    string `json:"in_reply_to"`
	AckRequested bool   `json:"ack_requested"`
}

// StatusRequest carries the caller's session id so the daemon can return
// the per-session current source. Empty Session normalises to "default".
type StatusRequest struct {
	Session string `json:"session,omitempty"`
}

type StatusReply struct {
	DaemonPID          int        `json:"daemon_pid"`
	DaemonVersion      string     `json:"daemon_version,omitempty"`
	LoggedIn           bool       `json:"logged_in"`
	URL                string     `json:"url,omitempty"`
	KeyPrefix          string     `json:"key_prefix,omitempty"`
	AccountID              string     `json:"account_id,omitempty"`
	AccountName            string     `json:"account_name,omitempty"`
	LastTokenRefreshAt *time.Time `json:"last_token_refresh_at,omitempty"`
	// LoginCheck is the daemon's last verification result against the
	// server. "ok" means a recent server-touching call succeeded;
	// "invalid" means the server returned E_INVALID_API_KEY (key
	// revoked / rotated since login). Empty / "unknown" means we
	// haven't observed yet — status performs an active probe in that
	// case so the user always sees the truth.
	LoginCheck string `json:"login_check,omitempty"`
	Current    string `json:"current,omitempty"`
	// CurrentNamespace is the per-session manifold (Phase 1.5). Empty
	// when unset = root namespace. Rendered as a `namespace: …` line by
	// `ppz status`.
	CurrentNamespace string `json:"current_namespace,omitempty"`
	// CurrentPath is the daemon-side path to current.json — surfaced so
	// `ppz status`'s env/daemon-disagree warning can point users at the
	// actual file (which lives in the daemon's home, not the CLI's, when
	// they're separate processes).
	CurrentPath string `json:"current_path,omitempty"`
	// NATSState is one of "connected", "disconnected", "connecting" —
	// the daemon's current NATS connection state. Empty means
	// unobserved (no connection ever attempted, e.g. fresh daemon
	// pre-login). Drives the `nats:` line in `ppz status` output;
	// underlying event log is available via `ppz diagnostics`. (Phase 0 of
	// agent hardening, docs/WIRE.md §8.)
	NATSState string `json:"nats_state,omitempty"`
}

// LoginCheck values reported by StatusReply. Constants live in cliproto
// so both daemon (writer) and CLI (reader) share them without import
// cycles. "" is the zero value and means "not applicable" (e.g. no
// credentials stored).
const (
	LoginCheckOK      = "ok"
	LoginCheckInvalid = "invalid"
)

type LoginRequest struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
}

type LoginReply struct {
	URL       string `json:"url"`
	KeyPrefix string `json:"key_prefix"`
	AccountID     string `json:"account_id"`
}

type CreateRequest struct {
	Handle   string `json:"handle"`
	Kind     string `json:"kind,omitempty"`     // "message" (default) or "pty"
	Manifold string `json:"manifold,omitempty"` // Phase 1.5.1: namespace-aware source create
	Session  string `json:"session,omitempty"`  // sets per-session current after create
}

type CreateReply struct {
	Handle   string   `json:"handle"`
	Manifold string   `json:"manifold,omitempty"` // Phase 1.5.1
	Subject  string   `json:"subject"`
	Pipes    []string `json:"pipes,omitempty"` // pipe names provisioned for this source
}

type SwitchRequest struct {
	Handle  string `json:"handle"`
	Session string `json:"session,omitempty"`
}

type SwitchReply struct {
	Handle string `json:"handle"`
}

// Phase 1.5: namespace (manifold) per-session state.

type SetNamespaceRequest struct {
	Namespace string `json:"namespace"` // dot-separated path; '' = root (clear)
	Session   string `json:"session,omitempty"`
}

type SetNamespaceReply struct {
	Namespace string `json:"namespace"`
}

type UnsetNamespaceRequest struct {
	Session string `json:"session,omitempty"`
}

type UnsetNamespaceReply struct{}

// ConnectRequest is the input to `ppz connect <handle>`. The daemon ensures
// the source exists (idempotent — pre-existing source is fine), then sets
// it as `current`.
type ConnectRequest struct {
	Handle  string `json:"handle"`
	Session string `json:"session,omitempty"`
}

type ConnectReply struct {
	Handle string `json:"handle"`
}

// DisconnectRequest carries the session id whose current source should be
// cleared. Empty Session normalises to "default".
type DisconnectRequest struct {
	Session string `json:"session,omitempty"`
}

// DisconnectReply is empty — the only outcome of disconnect is "current"
// being cleared on the daemon side. Returns no fields.
type DisconnectReply struct{}

type SendRequest struct {
	// Optional explicit target. If both empty, daemon publishes to its
	// current source on .broadcast. If Handle is set, publishes to
	// <Handle>.<Channel|"broadcast">. Used by `ppz send` and by
	// `ppz broadcast` when PPZ_CURRENT_HANDLE is exported (e.g. inside
	// a `ppz terminal` child).
	//
	// `channel` JSON tag preserved for IPC backward-compat within Phase A;
	// it carries a pipe name. Phase B reorganises.
	Handle  string `json:"handle,omitempty"`
	Channel string `json:"channel,omitempty"`
	Payload string `json:"payload"`
	// MsgSubject is an optional envelope-level subject (header-line). Free-
	// form for users (set via `ppz send --subject`); subjects starting with
	// `ack:` are reserved for daemon-internal protocol messages (ack
	// emission) and rejected at the IPC trust boundary in handleSend.
	MsgSubject string `json:"msg_subject,omitempty"`
	// InReplyTo / AckRequested mirror the new envelope fields (v0.25.0).
	// JSON tags align with the envelope (`in_reply_to`, `ack_requested`)
	// rather than the older `msg_subject` precedent — these are 1:1 with
	// envelope fields.
	InReplyTo    string `json:"in_reply_to,omitempty"`
	AckRequested bool   `json:"ack_requested,omitempty"`
	// Session keys the per-session current-source fallback when neither
	// Handle nor PPZ_CURRENT_HANDLE is set.
	Session string `json:"session,omitempty"`
	// Sender is the CLI's resolved current-handle hint, used to stamp
	// envelope.sender. The CLI is the only place PPZ_CURRENT_HANDLE
	// (set by `ppz terminal share` in the wrapped shell) can be read,
	// so it forwards the effective current as a hint and the daemon
	// honours it when its per-session State.Current is empty (the PTY-
	// share case: IPCCreate skips SetCurrent for PTY-kind sources, so
	// the daemon never learns the inner shell's current even though
	// the env says it's set). Empty Sender preserves today's behaviour:
	// daemon falls back to State.Current(Session).
	Sender string `json:"sender,omitempty"`

	// Phase 1.5: BareTarget carries the raw target string when the user
	// typed `ppz send LEAF` without a dot. The CLI mangles the bare form
	// to {Handle: LEAF, Channel: "inbox"} for backward compat with the
	// collared recipient.inbox shorthand; BareTarget lets the daemon
	// recognise the original bare form and fall back to uncollared pipe
	// resolution if the source handle lookup misses. Empty when the user
	// typed an explicit `<H>.<P>` form.
	BareTarget string `json:"bare_target,omitempty"`
}

type SendReply struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Bytes   int    `json:"bytes"`
}

// SendBatchRequest publishes N payloads in one IPC round-trip.
// Used by streaming producers (terminal share's stdout drain,
// `ppz broadcast` line-streaming) where the per-call NATS round-
// trip cost dominates throughput under WAN. Validation runs once
// for the whole batch; the daemon issues N async nc.Publish calls
// followed by ONE nc.Flush, then replies with N ids — preserving
// the same "bytes confirmed at server" contract as the single
// IPCSend call, just amortised across the batch.
type SendBatchRequest struct {
	Handle     string   `json:"handle,omitempty"`
	Channel    string   `json:"channel,omitempty"`
	BareTarget string   `json:"bare_target,omitempty"` // Phase 1.5: see SendRequest.BareTarget
	Payloads   []string `json:"payloads"`
	Session    string   `json:"session,omitempty"`
	// No Sender field: the only batch caller is `ppz terminal share`'s
	// stdout/stdctrl publisher (sendStreamBatch / sendStreamLine), which
	// runs in the share *parent* process — not the wrapped child. The
	// parent's PPZ_CURRENT_HANDLE is the outer shell's current, NOT the
	// wrapped handle, so forwarding it would stamp the wrong sender on
	// <handle>.stdout messages. The daemon's State.Current(session)
	// fallback (via senderForRequest) is the right resolution here.
}

// SendBatchReply mirrors SendReply but as parallel arrays,
// one entry per published payload. IDs[i] / Bytes[i] correspond to
// Payloads[i] in the request. Subject is shared across the batch
// (all messages land on the same handle.pipe).
type SendBatchReply struct {
	IDs     []string `json:"ids"`
	Subject string   `json:"subject"`
	Bytes   []int    `json:"bytes"`
}

// PipeInfo is per-pipe state surfaced by ppz ls. Total + LastSeq come from
// the JetStream stream's Info; Unread is computed daemon-side from the
// session's cursor file. Preview is the most recent payload truncated to
// 60 chars (UTF-8 safe; ANSI CSI sequences and C0 controls stripped).
//
// CreatedBy is the username of the user who created this pipe. Empty for
// auto-provisioned pipes (broadcast / inbox / stdin / stdout / stdctrl) —
// the renderer falls back to the source's CreatedBy when this field is
// empty so CREATOR is never blank in the output. omitempty keeps the wire
// shape clean when a daemon-side intermediate doesn't carry the join.
type PipeInfo struct {
	Pipe      string     `json:"pipe"`
	Total     uint64     `json:"total"`
	Unread    uint64     `json:"unread"`
	LastSeq   uint64     `json:"last_seq,omitempty"`
	LastAt    *time.Time `json:"last_at,omitempty"`
	Preview   string     `json:"preview,omitempty"`    // truncated to 60 bytes for table view
	Payload   string     `json:"payload,omitempty"`    // full untruncated payload for `ls --json`
	CreatedBy string     `json:"created_by,omitempty"` // username; empty → inherit Source.CreatedBy
}

// Source carries the source-level fields ppz ls renders. CreatedBy is the
// username of the user who created the source; populated server-side by
// joining sources.created_by_user_id to users.username.
type Source struct {
	Handle   string `json:"handle"`
	Manifold string `json:"manifold,omitempty"` // Phase 1.5.1
	Kind     string `json:"kind,omitempty"`
	// Pipes is the list of user-created pipe names on this source (NOT the
	// auto-provisioned set — derive those from Kind). Set by the server's
	// /api/v1/sources response.
	Pipes     []string   `json:"pipes,omitempty"`
	PipeInfos []PipeInfo `json:"pipe_infos,omitempty"`
	CreatedBy string     `json:"created_by,omitempty"`
	// Legacy broadcast-only summary; kept for the GUI handlers that still
	// read these directly from the postgres-backed sources table.
	LastBroadcastAt      *time.Time `json:"last_broadcast_at,omitempty"`
	LastBroadcastPayload *string    `json:"last_broadcast_payload,omitempty"`
}

type ListRequest struct {
	Session string `json:"session,omitempty"` // cursor key
}

// ListWatchRequest is `ppz ls --watch`. The daemon returns the same shape
// as ListReply, but only after the calling session has at least one
// unread message on a pipe whose handle matches one of Patterns (or any
// handle if Patterns is empty).
//
// Semantics: level-triggered. If unread > 0 already at call time on a
// matching handle, the daemon returns immediately. Otherwise it blocks
// until a new message arrives on a matching subject, then returns.
//
// Patterns are filepath.Match-style globs (`*`, `?`, `[abc]`) matched
// against the handle (not handle.pipe). Multiple patterns OR-combine.
type ListWatchRequest struct {
	Session  string   `json:"session,omitempty"`
	Patterns []string `json:"patterns,omitempty"`
}

type ListReply struct {
	Sources []Source `json:"sources"`
	// Phase 1.5: uncollared (sourceless) pipes — pipes with source_id IS
	// NULL. Walking sources alone misses them. The daemon enriches each
	// row with JetStream stats the same way it does PipeInfos.
	UncollaredPipes []UncollaredPipe `json:"uncollared_pipes,omitempty"`
}

// UncollaredPipe is the wire projection of a sourceless pipe row + its
// JetStream stats. Phase 1.5.
type UncollaredPipe struct {
	Manifold string `json:"manifold,omitempty"` // '' = root namespace
	Name     string `json:"name"`
	Info     PipeInfo `json:"info"`
}

// ListUncollaredPipesReply is the server response for GET /api/v1/pipes
// (uncollared listing). One entry per sourceless pipe in the account.
// Phase 1.5.
type ListUncollaredPipesReply struct {
	Pipes []UncollaredPipeListEntry `json:"pipes"`
}

type UncollaredPipeListEntry struct {
	Manifold  string `json:"manifold,omitempty"`
	Name      string `json:"name"`
	CreatedBy string `json:"created_by,omitempty"`
}

// Server HTTP types.

type AuthExchangeRequest struct {
	APIKey string `json:"api_key"`
	// AccountID (Phase 3.5): which org's account to mint a User JWT in.
	// Optional — server defaults to the bearer's primary org (first
	// owned, or first member). Multi-org users specify it explicitly
	// to switch which org their daemon talks to.
	AccountID string `json:"account_id,omitempty"`
}

// CreateInviteRequest is the body for POST /api/v1/orgs/{slug}/invites
// and the /orgs/{id}/invites GUI form. Owner-only — handlers gate.
type CreateInviteRequest struct {
	Username string `json:"username"`
}

// Invite is the API projection of a db.Invite row plus the account name
// (so the dashboard can render "Pending invitation to <account>" without
// a second join).
type Invite struct {
	ID              string `json:"id"`
	AccountID       string `json:"account_id"`
	AccountName     string `json:"account_name,omitempty"`
	InviteeUsername string `json:"invitee_username"`
	InviterUserID   string `json:"inviter_user_id"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	DecidedAt       string `json:"decided_at,omitempty"`
}

type CreateInviteReply struct {
	Invite Invite `json:"invite"`
}

type ListInvitesReply struct {
	Invites []Invite `json:"invites"`
}


type AuthExchangeReply struct {
	JWT       string    `json:"jwt"`
	NATSURL   string    `json:"nats_url"`
	AccountID     string    `json:"account_id"`
	AccountName   string    `json:"account_name"`
	ExpiresAt time.Time `json:"expires_at"`

	// Auth V2 Phase 3 — short-lived NATS user credentials. The daemon
	// uses NATSUserJWT + NATSUserSeed in nats.UserJWT(...) when
	// connecting to the NATS server URL. Re-fetch before ExpiresAt
	// (currently 5min) by re-running /auth/exchange with the same
	// bearer.
	NATSUserJWT  string `json:"nats_user_jwt"`
	NATSUserSeed string `json:"nats_user_seed"`
}

type CreateSourceRequest struct {
	Handle   string `json:"handle"`
	Kind     string `json:"kind,omitempty"`     // "message" (default) or "pty"
	Manifold string `json:"manifold,omitempty"` // Phase 1.5.1: source lives at this manifold
}

type CreateSourceReply struct {
	ID        string    `json:"id"`
	Handle    string    `json:"handle"`
	Manifold  string    `json:"manifold,omitempty"` // Phase 1.5.1
	Kind      string    `json:"kind"`
	Subject   string    `json:"subject"`
	CreatedAt time.Time `json:"created_at"`
}

type ListSourcesReply struct {
	Sources []Source `json:"sources"`
}

// PipeCreateRequest is the input to `ppz pipe create <name>` — and the body
// of POST /api/v1/sources/{handle}/pipes and POST /api/v1/pipes (Phase 1.5
// sourceless form). Retention overrides are pointers so "absent" (= use
// default) is distinguishable from "explicitly zero".
//
// Phase 1.5 fields per locked decision #18 four-role grammar:
//   - Manifold:     hierarchical-grouping segment string ('' = root)
//   - SourceHandle: actor identity name; nil = uncollared (sourceless)
//
// Handle is retained as a backward-compat alias for SourceHandle until
// Cycle B finishes threading the new fields through the daemon and CLI;
// Cycle E's docs commit removes it.
type PipeCreateRequest struct {
	Handle       string  `json:"handle"`
	Manifold     string  `json:"manifold,omitempty"`
	SourceHandle *string `json:"source_handle,omitempty"`
	Name         string  `json:"name"`
	TTLSeconds   *int    `json:"ttl_seconds,omitempty"`
	MaxMsgs      *int    `json:"max_msgs,omitempty"`
	MaxBytes     *int64  `json:"max_bytes,omitempty"`

	// Session is set by the CLI for daemon-side manifold lookup; the IPC
	// transport drops it before forwarding to the server. Phase 1.5.
	Session string `json:"session,omitempty"`
}

// PipeCreateReply mirrors the server's resolved retention (after defaults
// are filled in) so the CLI prints exactly what was provisioned.
type PipeCreateReply struct {
	Handle     string `json:"handle"`
	Manifold   string `json:"manifold,omitempty"` // Phase 1.5
	Name       string `json:"name"`
	StreamName string `json:"stream_name"`
	TTLSeconds int    `json:"ttl_seconds"`
	MaxMsgs    int    `json:"max_msgs"`
	MaxBytes   int64  `json:"max_bytes"`
}

type PipeDestroyRequest struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
	// Phase 1.5: BareTarget set by the CLI when the user typed
	// `ppz pipe destroy LEAF` without a dot. Daemon resolves as an
	// uncollared pipe at the session's current namespace.
	BareTarget string `json:"bare_target,omitempty"`
	Session    string `json:"session,omitempty"`
	// Manifold is set by callers that already know the target pipe's
	// manifold (e.g. the glob path needs to destroy uncollared pipes
	// across manifolds, not just the session's). When empty, the
	// daemon falls back to the session's current_namespace.
	Manifold string `json:"manifold,omitempty"`
}

type PipeDestroyReply struct {
	Handle   string `json:"handle"`
	Manifold string `json:"manifold,omitempty"` // Phase 1.5: present for uncollared destroys
	Name     string `json:"name"`
}

type SourceDestroyRequest struct {
	Handle string `json:"handle"`
}

type SourceDestroyReply struct {
	Handle   string `json:"handle"`
	Manifold string `json:"manifold,omitempty"` // Phase 1.5.2: render manifold.handle in display
}

// HTTPError is the body shape of a non-2xx HTTP response.
type HTTPError struct {
	Error Error `json:"error"`
}

// DiagRequest is the input to `ppz diagnostics` — currently empty. Reserved
// for future scoping flags (per-subsystem filters, since-when, etc.).
type DiagRequest struct{}

// DiagEvent is one connection-state transition in DiagReply. Fields
// mirror the daemon's NATSEvent struct (kept as a separate type so
// the IPC contract is independent of internal storage).
type DiagEvent struct {
	Type   string    `json:"type"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// DiagReply carries the daemon's introspection snapshot. Phase 0:
// just the NATS connection state + recent connection-state events.
// Future phases will extend with refresh-loop state, JetStream
// consumer lag, etc.
type DiagReply struct {
	NATSState         string      `json:"nats_state,omitempty"`
	NATSDropsLastHour int         `json:"nats_drops_last_hour,omitempty"`
	NATSEvents        []DiagEvent `json:"nats_events"`
}

// WhoRequest is the input to `ppz who`. Empty for v1 — filters are
// applied client-side so the daemon stays a pure snapshot provider.
type WhoRequest struct{}

// WhoEntry is one row of the daemon's heartbeat cache. Payload is the
// verbatim heartbeat JSON the pty wrapper published; consumers
// (cmdWho) unmarshal it as HeartbeatPayload to extract harness/model/
// host fields.
//
// Owner is the username that owns the underlying source — resolved at
// query time from the server's source listing rather than embedded in
// the heartbeat payload, so transferring ownership server-side
// reflects in `ppz who` on the next call without restarting the
// agent. Empty when the daemon couldn't reach the server, or when the
// cache has a beat for a source the server no longer knows about.
type WhoEntry struct {
	Handle    string    `json:"handle"`
	Owner     string    `json:"owner,omitempty"`
	Payload   string    `json:"payload"`
	ArrivedAt time.Time `json:"arrived_at"`
}

// WhoReply carries the daemon's sorted-by-handle snapshot of every
// known heartbeat. Lifetime is the daemon process — a restart clears
// the cache and the next round of beats re-populates it.
type WhoReply struct {
	Entries []WhoEntry `json:"entries"`
}
