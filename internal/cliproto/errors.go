// Package cliproto holds the contracts shared by the CLI, daemon, server,
// and desktop: error/exit codes, stdout printers, and IPC types.
//
// Everything in this package mirrors docs/ERRORS.md and docs/WIRE.md. If you
// change a string here, change those docs too — and the test fixtures.
package cliproto

import "fmt"

// Code is a stable error identifier. Strings are part of the wire contract.
type Code string

const (
	ENotLoggedIn       Code = "E_NOT_LOGGED_IN"
	EDaemonNotRunning  Code = "E_DAEMON_NOT_RUNNING"
	// EDaemonTimeout is returned by the CLI when the daemon accepted the
	// IPC connection but did not reply within the client deadline. Per
	// the send delivery contract clause 2, the CLI must never hang: a
	// stalled daemon (e.g. mid-restart, before it serves IPC) bounds out
	// to this error instead of blocking forever.
	EDaemonTimeout Code = "E_DAEMON_TIMEOUT"
	EInvalidAPIKey     Code = "E_INVALID_API_KEY"
	ESourceTaken       Code = "E_SOURCE_TAKEN"
	ESourceNotFound    Code = "E_SOURCE_NOT_FOUND"
	EInvalidHandle     Code = "E_INVALID_HANDLE"
	ENoCurrentSource   Code = "E_NO_CURRENT_SOURCE"
	EPayloadTooLarge   Code = "E_PAYLOAD_TOO_LARGE"
	EServerUnreachable Code = "E_SERVER_UNREACHABLE"
	ENATSUnreachable   Code = "E_NATS_UNREACHABLE"
	EInvalidPipe       Code = "E_INVALID_PIPE"
	EPipeTaken         Code = "E_PIPE_TAKEN"
	EPipeNotFound      Code = "E_PIPE_NOT_FOUND"
	// EInvalidSubject is returned when a caller (CLI flag parser or IPC
	// client) tries to set a Subject value that violates the reserved-
	// prefix invariant. Daemon-emitted protocol messages own the `ack:`
	// prefix; users cannot stamp it themselves.
	EInvalidSubject Code = "E_INVALID_SUBJECT"

	// EInvalidManifold is returned when a manifold (hierarchical-grouping
	// path) segment fails validation. Each dot-separated segment must
	// match the handle regex (lowercase alnum + hyphens, max 32, no
	// leading/trailing hyphen). Phase 1.5.
	EInvalidManifold Code = "E_INVALID_MANIFOLD"

	// EDeliveryUnconfirmed is returned when `ppz send` published a
	// message but did not receive a JetStream PubAck within the deadline.
	// The message MAY or MAY NOT have landed — distinct from
	// ENATSUnreachable ("never left") so callers can decide whether to
	// retry knowing a retry may duplicate. Per the send delivery
	// contract: exit 0 ONLY on a confirmed PubAck; unconfirmed is a
	// failure, never silent success.
	EDeliveryUnconfirmed Code = "E_DELIVERY_UNCONFIRMED"

	// ENameTaken — Phase 1.5.1 first-wins collision rule. Within a
	// manifold, user-typed names share a namespace across source
	// handles and uncollared pipe names; a source at manifold M also
	// reserves the manifold-prefix path M.<handle> from any uncollared
	// pipe creation. Surfaces when a create would conflict with an
	// existing row of either shape.
	ENameTaken Code = "E_NAME_TAKEN"
)

// ExitCode returns the integer the CLI exits with for a given Code. Unknown
// codes map to 1 (generic error).
func ExitCode(c Code) int {
	switch c {
	case ENotLoggedIn:
		return 10
	case EDaemonNotRunning:
		return 11
	case EInvalidAPIKey:
		return 12
	case ESourceTaken:
		return 13
	case ESourceNotFound:
		return 14
	case EInvalidHandle:
		return 15
	case ENoCurrentSource:
		return 16
	case EPayloadTooLarge:
		return 17
	case EServerUnreachable:
		return 18
	case ENATSUnreachable:
		return 19
	case EInvalidPipe:
		return 20
	case EPipeTaken:
		return 21
	case EPipeNotFound:
		return 22
	case EInvalidSubject:
		return 23
	case EInvalidManifold:
		return 24
	case ENameTaken:
		return 21
	case EDeliveryUnconfirmed:
		return 25
	case EDaemonTimeout:
		return 26
	}
	return 1
}

// Message is the standard human-readable message for a Code, per ERRORS.md.
func Message(c Code) string {
	switch c {
	case ENotLoggedIn:
		return "not logged in; run 'ppz daemon login URL -apikey K'"
	case EDaemonNotRunning:
		return "daemon not running; run 'ppz daemon start'"
	case EInvalidAPIKey:
		return "invalid api key"
	case ESourceTaken:
		return "source already exists in this account"
	case ESourceNotFound:
		return "source not found"
	case EInvalidHandle:
		return "invalid handle: must match [a-z0-9-] (max 32, no leading/trailing -, not reserved)"
	case ENoCurrentSource:
		return "no current source for this shell session; run 'ppz terminal create <handle>' (or 'ppz set handle <handle>' to point at an existing one); if you're driving ppz from agent subprocesses with no shared tty, export PPZ_SESSION=<id> consistently across calls so they share session state"
	case EPayloadTooLarge:
		return "payload too large; max 64KiB encoded"
	case EServerUnreachable:
		return "server unreachable"
	case ENATSUnreachable:
		// MoltHub 2026-05-08: most-common cause was expired credentials,
		// not URL misconfig. Lead with that; keep PPZ_NATS_URL guidance
		// for non-docker setups.
		return "nats unreachable; common causes: expired credentials (try 'ppz daemon logout' then re-login), or on non-docker setups missing PPZ_NATS_URL=nats://localhost:4222"
	case EInvalidPipe:
		// MoltHub 2026-05-08: enumerating only the four built-in pipes
		// misled agents into thinking custom pipes were unsupported.
		// Cover both common causes (typo / missing custom pipe) and
		// point at the actionable command.
		return "invalid pipe; check for typos, or for custom pipes run 'ppz pipe create <handle>.<name>' first"
	case EPipeTaken:
		return "pipe with this name already exists on this source"
	case EPipeNotFound:
		return "pipe not found on this source"
	case EInvalidSubject:
		return "invalid subject; the 'ack:' prefix is reserved for system-emitted protocol messages"
	case EInvalidManifold:
		return "invalid manifold: each dot-separated segment must match [a-z0-9-] (max 32, no leading/trailing -, not reserved)"
	case ENameTaken:
		return "name already taken by another resource at this manifold"
	case EDeliveryUnconfirmed:
		return "delivery unconfirmed; the message was published but the server did not acknowledge it in time — it may or may not have landed; retry if your workflow tolerates a possible duplicate"
	case EDaemonTimeout:
		return "daemon did not respond in time; it may be busy or stuck (e.g. mid-restart) — retry, or run 'ppz daemon restart'"
	}
	return "unknown error"
}

// Error is a wire-level error carrying a Code and an arbitrary message.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// New builds an *Error using the standard Message for the code.
func New(c Code) *Error {
	return &Error{Code: c, Message: Message(c)}
}

// Constructors that thread the offending entity name into the message —
// "source not found" is useless without saying which source. Prefer these
// over plain New(...) wherever the caller has the name.

// NewSourceNotFound: source 'foo' not found
func NewSourceNotFound(handle string) *Error {
	return &Error{Code: ESourceNotFound, Message: fmt.Sprintf("source '%s' not found", handle)}
}

// NewSourceTaken: source 'foo' already exists in this account
func NewSourceTaken(handle string) *Error {
	return &Error{Code: ESourceTaken, Message: fmt.Sprintf("source '%s' already exists in this account", handle)}
}

// NewPipeTaken: pipe 'archive' already exists on source 'foo'
func NewPipeTaken(pipe, handle string) *Error {
	return &Error{Code: EPipeTaken, Message: fmt.Sprintf("pipe '%s' already exists on source '%s'", pipe, handle)}
}

// NewPipeNotFound: pipe 'archive' not found on source 'foo'
func NewPipeNotFound(pipe, handle string) *Error {
	return &Error{Code: EPipeNotFound, Message: fmt.Sprintf("pipe '%s' not found on source '%s'", pipe, handle)}
}

// NewUncollaredPipeNotFound: uncollared pipe 'room' not found at manifold ''
// (or at manifold 'team-a'). Phase 1.5 — avoids rendering an empty
// "on source ''" tail for sourceless pipes.
func NewUncollaredPipeNotFound(name, manifold string) *Error {
	location := "root"
	if manifold != "" {
		location = fmt.Sprintf("manifold '%s'", manifold)
	}
	return &Error{Code: EPipeNotFound, Message: fmt.Sprintf("uncollared pipe '%s' not found at %s", name, location)}
}

// NewUncollaredPipeTaken: uncollared pipe 'archive' already exists at root
// (or at manifold 'team-a'). Phase 1.5.1 — companion to NewUncollaredPipeNotFound.
func NewUncollaredPipeTaken(name, manifold string) *Error {
	location := "root"
	if manifold != "" {
		location = fmt.Sprintf("manifold '%s'", manifold)
	}
	return &Error{Code: EPipeTaken, Message: fmt.Sprintf("uncollared pipe '%s' already exists at %s", name, location)}
}

// NewNameTakenBySource: name 'foo' is already taken by source at root
// (or at manifold 'team-a'). Phase 1.5.1 collision rule.
func NewNameTakenBySource(name, manifold string) *Error {
	location := "root"
	if manifold != "" {
		location = fmt.Sprintf("manifold '%s'", manifold)
	}
	return &Error{Code: ENameTaken, Message: fmt.Sprintf("name '%s' is already taken by source at %s", name, location)}
}

// NewNameTakenByUncollaredPipe: name 'foo' is already taken by
// uncollared pipe at root (or manifold). Phase 1.5.1 collision rule.
func NewNameTakenByUncollaredPipe(name, manifold string) *Error {
	location := "root"
	if manifold != "" {
		location = fmt.Sprintf("manifold '%s'", manifold)
	}
	return &Error{Code: ENameTaken, Message: fmt.Sprintf("name '%s' is already taken by uncollared pipe at %s", name, location)}
}

// NewManifoldReservedBySource: manifold path 'team1' is reserved by source 'team1'
// at root. Phase 1.5.1 — source X at manifold M reserves the manifold-prefix
// path M.X because the source's auto-pipes already live at those subjects.
// `prefix` is the colliding manifold path (e.g. "team1.subteam"). `sourceHandle`
// is the source's bare name (e.g. "subteam"). `sourceManifold` is where the
// source lives (e.g. "team1", or "" for root).
func NewManifoldReservedBySource(prefix, sourceHandle, sourceManifold string) *Error {
	location := "root"
	if sourceManifold != "" {
		location = fmt.Sprintf("manifold '%s'", sourceManifold)
	}
	return &Error{Code: ENameTaken, Message: fmt.Sprintf("manifold path '%s' is reserved by source '%s' at %s", prefix, sourceHandle, location)}
}

// NewInvalidPipeReserved: pipe name 'system' is reserved
func NewInvalidPipeReserved(name string) *Error {
	return &Error{Code: EInvalidPipe, Message: fmt.Sprintf("pipe name '%s' is reserved", name)}
}

// NewInvalidPipeName: pipe name 'X' is invalid (regex rejection / etc.)
func NewInvalidPipeName(name string) *Error {
	return &Error{Code: EInvalidPipe, Message: fmt.Sprintf("pipe name '%s' is invalid: must match [a-z0-9-] (max 32, no leading/trailing -)", name)}
}

// NewInvalidHandle: invalid handle 'BAD'
func NewInvalidHandle(handle string) *Error {
	return &Error{Code: EInvalidHandle, Message: fmt.Sprintf("invalid handle '%s': must match [a-z0-9-] (max 32, no leading/trailing -, not reserved)", handle)}
}

// HTTPStatus returns the HTTP status code for a given Code, or 500 if unknown.
func HTTPStatus(c Code) int {
	switch c {
	case EInvalidAPIKey:
		return 401
	case ESourceTaken:
		return 409
	case ESourceNotFound:
		return 404
	case EInvalidHandle:
		return 400
	case EPayloadTooLarge:
		return 413
	case EServerUnreachable:
		return 502
	case EInvalidPipe:
		return 400
	case EPipeTaken:
		return 409
	case EPipeNotFound:
		return 404
	case EInvalidSubject:
		return 400
	case EInvalidManifold:
		return 400
	case ENameTaken:
		return 409
	}
	return 500
}
