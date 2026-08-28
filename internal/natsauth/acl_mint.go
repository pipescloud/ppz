package natsauth

// Minting a credential from a compiled ACL — Phase 3.
//
// MintUserJWTInAccount takes bare allow lists; enforcement also needs
// deny lists, and deny is not optional decoration: stream enumeration
// carries no stream token, so it can only be shut off by denying it.

import (
	"fmt"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"

	"github.com/pipescloud/ppz/internal/acl"
)

// MintUserJWTWithPermissions signs a user JWT carrying exactly the
// permissions the ACL compiler produced.
func MintUserJWTWithPermissions(accountPub string, signingKP nkeys.KeyPair, name string, p acl.Permissions, expUnix int64) (jwtStr, seed string, err error) {
	userKP, err := nkeys.CreateUser()
	if err != nil {
		return "", "", fmt.Errorf("create user nkey: %w", err)
	}
	userPub, err := userKP.PublicKey()
	if err != nil {
		return "", "", fmt.Errorf("user pub: %w", err)
	}
	seedBytes, err := userKP.Seed()
	if err != nil {
		return "", "", fmt.Errorf("user seed: %w", err)
	}

	claims := jwt.NewUserClaims(userPub)
	claims.Name = name
	claims.IssuerAccount = accountPub
	claims.Pub.Allow = p.PubAllow
	claims.Pub.Deny = p.PubDeny
	claims.Sub.Allow = p.SubAllow
	claims.Sub.Deny = p.SubDeny

	now := time.Now()
	claims.IssuedAt = now.Unix()
	claims.Expires = expUnix
	claims.NotBefore = now.Add(-30 * time.Second).Unix()

	encoded, err := claims.Encode(signingKP)
	if err != nil {
		return "", "", fmt.Errorf("encode user jwt: %w", err)
	}
	return encoded, string(seedBytes), nil
}
