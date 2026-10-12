package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// handleGUIPipePage renders every buffered message on a (source, pipe) pair.
// Reads from the JetStream stream backing the pipe; messages are returned in
// chronological order (sequence ASC).
//
// Route: GET /orgs/{id}/sources/{handle}/pipes/{pipe}
// {id} accepts either UUID or slug via resolveOrg.
func (s *Server) handleGUIPipePage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()

	org, ok := s.resolveMemberOrg(ctx, w, r)
	if !ok {
		return
	}
	handle := r.PathValue("handle")
	pipeName := r.PathValue("pipe")
	if err := natsubj.ValidatePipe(pipeName); err != nil {
		http.Error(w, "unknown pipe", 404)
		return
	}

	// Verify the source exists in this org. Without this we'd happily render
	// streams from any org if you guessed the handle.
	src, err := db.GetSourceByHandle(ctx, s.Pool, org.ID, handle)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.Error(w, "source not found", 404)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}

	subject := natsubj.BuildSubject(org.ID, src.Manifold, src.Handle, pipeName)
	streamName := natsubj.BuildStreamName(org.ID, src.Manifold, src.Handle, pipeName)

	type messageView struct {
		ID        string
		CreatedAt string
		Payload   string
	}
	var messages []messageView

	js, _ := s.JSFor(ctx, org.ID)
	if js == nil {
		// Org account not provisioned yet (just-created org, no
		// streams yet). Render the page with no buffered messages.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.ExecuteTemplate(w, "channel.html", map[string]any{
			"Org": org, "Source": src, "Pipe": pipeName, "Subject": subject, "Messages": messages,
		})
		return
	}
	stream, err := js.Stream(ctx, streamName)
	if err == nil {
		info, err := stream.Info(ctx)
		if err == nil && info.State.Msgs > 0 {
			for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
				msg, err := stream.GetMsg(ctx, seq)
				if err != nil {
					// Sequence gap (expired by MaxAge / dropped by limits).
					continue
				}
				env, err := envelope.Unmarshal(msg.Data)
				if err != nil {
					continue
				}
				messages = append(messages, messageView{
					ID:        env.ID,
					CreatedAt: env.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
					Payload:   env.Payload,
				})
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "channel.html", map[string]any{
		"Org":      org,
		"Handle":   src.Handle,
		"Pipe":     pipeName,
		"Subject":  subject,
		"Messages": messages,
	}); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// handleGUIUncollaredPipePage is the sourceless counterpart to
// handleGUIPipePage. Uncollared pipes don't hang off a source row, so
// they live at /orgs/{id}/pipes/{pipe} — where {pipe} is the dotted
// "<manifold>.<name>" path the org page surfaces (manifold may be
// empty, in which case it's just "<name>"). Last dot splits the two.
//
// Route: GET /orgs/{id}/pipes/{pipe}
func (s *Server) handleGUIUncollaredPipePage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()

	org, ok := s.resolveMemberOrg(ctx, w, r)
	if !ok {
		return
	}
	pipePath := r.PathValue("pipe")
	manifold, name := splitManifoldName(pipePath)
	if err := natsubj.ValidatePipe(name); err != nil {
		http.Error(w, "unknown pipe", 404)
		return
	}
	if manifold != "" {
		for _, seg := range strings.Split(manifold, ".") {
			if err := natsubj.ValidateHandle(seg); err != nil {
				http.Error(w, "unknown pipe", 404)
				return
			}
		}
	}

	// Confirm the uncollared pipe row exists in this org. Without
	// this, any URL with a syntactically-valid path would render a
	// blank page even when no pipe was ever created — surfaces as
	// 404 instead, matching the collared handler's source check.
	exists, err := db.UncollaredPipeExists(ctx, s.Pool, org.ID, manifold, name)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if !exists {
		http.Error(w, "pipe not found", 404)
		return
	}

	subject := natsubj.BuildSubject(org.ID, manifold, "", name)
	streamName := natsubj.BuildStreamName(org.ID, manifold, "", name)

	type messageView struct {
		ID        string
		CreatedAt string
		Payload   string
	}
	var messages []messageView

	js, _ := s.JSFor(ctx, org.ID)
	if js != nil {
		stream, err := js.Stream(ctx, streamName)
		if err == nil {
			info, err := stream.Info(ctx)
			if err == nil && info.State.Msgs > 0 {
				for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
					msg, err := stream.GetMsg(ctx, seq)
					if err != nil {
						continue
					}
					env, err := envelope.Unmarshal(msg.Data)
					if err != nil {
						continue
					}
					messages = append(messages, messageView{
						ID:        env.ID,
						CreatedAt: env.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
						Payload:   env.Payload,
					})
				}
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "channel.html", map[string]any{
		"Org":      org,
		"Handle":   "",
		"Pipe":     pipePath,
		"Subject":  subject,
		"Messages": messages,
	}); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// splitManifoldName splits a dotted pipe path into (manifold, name).
// The leaf name is always the segment after the last dot; everything
// before is the manifold ("" for a root-manifold pipe).
func splitManifoldName(pipePath string) (manifold, name string) {
	idx := strings.LastIndex(pipePath, ".")
	if idx < 0 {
		return "", pipePath
	}
	return pipePath[:idx], pipePath[idx+1:]
}
