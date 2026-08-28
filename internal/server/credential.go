package server

// Credential shaping — ACL Phase 3.

import "github.com/pipescloud/ppz/internal/acl"

// credentialPermissions decides what goes into a caller's NATS
// credential.
//
// The flag is the whole gate. Every org ships with acl_enforced=false,
// so every org keeps getting the wide-open credential it has today —
// if that regresses, every existing deployment breaks the moment the
// release lands, before anyone has opted into anything.
func credentialPermissions(enforced bool, accountID string, access []acl.Access) acl.Permissions {
	if !enforced {
		return acl.Unrestricted()
	}
	return acl.Compile(accountID, access)
}
