package daemon

// ACL verbs — ACL Phase 2.
//
// A pure HTTP passthrough: the daemon holds the credential, so the CLI
// asks it to make the call, and the server's JSON comes back verbatim.
// No NATS, no daemon state, and no knowledge of the ACL model here —
// keeping the model in one place (internal/acl, evaluated server-side)
// is what stops the CLI and the GUI drifting into two answers.

import (
	"context"
	"encoding/json"
	"net"
	"net/url"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// aclMutation is the server's wire body. The IPC request carries an
// extra Action field to pick the endpoint, and the server decodes with
// DisallowUnknownFields — posting the IPC struct verbatim is a 400.
type aclMutation struct {
	Pipe      string `json:"pipe"`
	Principal string `json:"principal"`
	Perm      string `json:"perm"`
}

type enforceBody struct {
	Enforced bool `json:"enforced"`
}

func mutationBody(req cliproto.ACLRequest) aclMutation {
	return aclMutation{Pipe: req.Pipe, Principal: req.Principal, Perm: req.Perm}
}

func (d *Daemon) handleACL(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.ACLRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, cliproto.New(cliproto.EInvalidPipe))
		return
	}

	var body json.RawMessage
	var e *cliproto.Error
	switch req.Action {
	case cliproto.ACLActionRoster:
		e = d.callServer(ctx, "GET", "/api/v1/acl?pipe="+url.QueryEscape(req.Pipe), nil, &body)
	case cliproto.ACLActionPrincipal:
		e = d.callServer(ctx, "GET", "/api/v1/acl?principal="+url.QueryEscape(req.Principal), nil, &body)
	case cliproto.ACLActionWhoami:
		e = d.callServer(ctx, "GET", "/api/v1/acl/whoami?pipe="+url.QueryEscape(req.Pipe), nil, &body)
	case cliproto.ACLActionGrant:
		e = d.callServer(ctx, "POST", "/api/v1/acl/grant", mutationBody(req), &body)
	case cliproto.ACLActionRevoke:
		e = d.callServer(ctx, "POST", "/api/v1/acl/revoke", mutationBody(req), &body)
	case cliproto.ACLActionEnforceGet:
		e = d.callServer(ctx, "GET", "/api/v1/acl/enforce", nil, &body)
	case cliproto.ACLActionEnforceOn:
		e = d.callServer(ctx, "POST", "/api/v1/acl/enforce", enforceBody{Enforced: true}, &body)
	case cliproto.ACLActionEnforceOff:
		e = d.callServer(ctx, "POST", "/api/v1/acl/enforce", enforceBody{Enforced: false}, &body)
	case cliproto.ACLActionPreview:
		e = d.callServer(ctx, "GET", "/api/v1/acl/preview", nil, &body)
	default:
		writeIPCErr(conn, cliproto.New(cliproto.EInvalidPipe))
		return
	}
	if e != nil {
		writeIPCErr(conn, e)
		return
	}
	writeIPC(conn, cliproto.ACLReply{Body: body})
}
