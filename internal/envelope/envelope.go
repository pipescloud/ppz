// Package envelope is the JSON shape of every message published on
// <org_id>.<handle>.broadcast (per WIRE.md §3).
package envelope

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

const MaxBytes = 65536 // 64 KiB cap on the encoded envelope.

// Sender is the source handle the message was published *from* — the
// broadcaster's current source at publish time. Empty string when the
// publisher had no current source set (e.g. `ppz send <dest>` from a
// session that never connected). Distinct from the destination handle,
// which is encoded only in the NATS subject (per WIRE.md §3).
//
// Subject is an optional header-line, separate from the payload. Two
// roles:
//   - User-set: free-form text rendered as `[subject] payload` in the
//     `ppz read` tabular default for inbox-shaped pipes. No CLI surface
//     to set this in v0.23.0 — reserved for the next phase.
//   - System-set: subjects starting with `ack:` are reserved for daemon-
//     emitted protocol messages (e.g. `ack:read`) that the read
//     formatter renders specially. Always serialised, even when empty,
//     so receivers see a stable wire shape.
// InReplyTo carries the UUID of the message this one is replying to (e.g.
// the original message's id when the daemon auto-emits an `ack:read`).
// Empty string when this isn't a reply. Always serialised — receivers see
// every field, every time.
//
// AckRequested is the sender's opt-in to a daemon-emitted read receipt:
// when the recipient's `ppz read` advances the cursor past a message
// with this bit set, the recipient's daemon publishes an `ack:read`
// envelope back to the sender's `<sender>.inbox`. Best-effort, non-
// blocking: a failed ack publish does not block cursor advancement.
//
// Priority is the sender's delivery-order hint: 1=high, 2=medium,
// 3=low. 0 means unset (legacy envelopes, senders that didn't ask,
// daemon-emitted acks, batch frames) and readers treat it as medium.
// Only inbox-shaped drains sort on it; byte-faithful pipes ignore it.
// Always serialised, even when zero.
type Message struct {
	ID           string    `json:"id"`
	Sender       string    `json:"sender"`
	Subject      string    `json:"subject"`
	Payload      string    `json:"payload"`
	CreatedAt    time.Time `json:"created_at"`
	InReplyTo    string    `json:"in_reply_to"`
	AckRequested bool      `json:"ack_requested"`
	Priority     int       `json:"priority"`
}

func New(sender, subject, payload string, now time.Time) Message {
	return Message{
		ID:        uuid.NewString(),
		Sender:    sender,
		Subject:   subject,
		Payload:   payload,
		CreatedAt: now.UTC(),
	}
}

func (m Message) Marshal() ([]byte, error) {
	return json.Marshal(m)
}

func Unmarshal(b []byte) (Message, error) {
	var m Message
	err := json.Unmarshal(b, &m)
	return m, err
}
