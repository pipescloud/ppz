package natsauth

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// Regression: pipescloud production, 2026-08-31.
//
// ACL Phase 3 (internal/acl/compile.go) replaced the per-account
// wildcard `<account>.>` with an explicit per-subject allow-list. That
// list grows with handles × pipes, and the whole User JWT rides inside
// the NATS CONNECT protocol line — which nats-server bounds by
// max_control_line, default 4096 bytes.
//
// A four-handle org compiled to a 4493-byte JWT, so every connect died
// in the protocol parser with "maximum control line exceeded" before
// authentication ever ran. The daemon surfaced E_NATS_UNREACHABLE and
// advised re-login; re-login mints an equally oversized credential, so
// the org was unrecoverable from the client side.
//
// These tests pin the embedded server's control-line budget against a
// realistically-sized ACL credential.

// aclJSReadPatterns mirrors internal/acl.jsReadPatterns — the JetStream
// API subjects a read needs, one set per readable stream.
var aclJSReadPatterns = []string{
	"$JS.API.STREAM.INFO.%s",
	"$JS.API.STREAM.MSG.GET.%s",
	"$JS.API.DIRECT.GET.%s",
	"$JS.API.CONSUMER.CREATE.%s.>",
	"$JS.API.CONSUMER.MSG.NEXT.%s.>",
}

// aclShapedSubjects reproduces the compiled ACL permission shape: one
// account-scoped subject per pipe per handle, rather than one wildcard.
func aclShapedSubjects(accountID string, handles []string) (pub, sub []string) {
	pub = []string{"$JS.API.INFO"}
	sub = []string{"_INBOX.>", accountID + "._presence.>", accountID + "._system.>"}
	for _, h := range handles {
		for _, leaf := range []string{"stdin", "stdout", "stdctrl", "system", "inbox"} {
			s := fmt.Sprintf("%s.%s.%s", accountID, h, leaf)
			pub = append(pub, s)
			sub = append(sub, s)
		}
		p := fmt.Sprintf("%s._presence.%s", accountID, h)
		pub = append(pub, p)
		sub = append(sub, p)
		for _, pat := range aclJSReadPatterns {
			pub = append(pub, strings.Replace(pat, "%s", accountID+"_"+h+"_inbox", 1))
		}
	}
	return pub, sub
}

// startEmbeddedForTest boots the production embedded-server path.
func startEmbeddedForTest(t *testing.T, cfg EmbeddedConfig) (string, string, nkeys.KeyPair, func()) {
	t.Helper()
	chain, err := BootstrapOperator()
	if err != nil {
		t.Fatalf("bootstrap operator: %v", err)
	}
	opKP, err := nkeys.FromSeed([]byte(chain.OperatorSeed))
	if err != nil {
		t.Fatalf("operator seed: %v", err)
	}
	accKP, _ := nkeys.CreateAccount()
	accPub, _ := accKP.PublicKey()
	signKP, _ := nkeys.CreateAccount()
	accJWT, err := MintAccountJWT(opKP, "tenant", accKP, signKP)
	if err != nil {
		t.Fatalf("mint account: %v", err)
	}

	cfg.OperatorJWT = chain.OperatorJWT
	cfg.SystemAccountJWT = chain.SystemAccountJWT
	cfg.AccountJWT = accJWT
	cfg.Host = "127.0.0.1"

	ns, err := StartEmbeddedNATSWithAuth(cfg)
	if err != nil {
		t.Fatalf("start embedded nats: %v", err)
	}
	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		t.Fatalf("nats not ready for connections")
	}
	return ns.ClientURL(), accPub, signKP, ns.Shutdown
}

func connectWithSubjects(t *testing.T, url, accPub string, signKP nkeys.KeyPair, pub, sub []string) (int, error) {
	t.Helper()
	exp := time.Now().Add(5 * time.Minute).Unix()
	userJWT, seed, err := MintUserJWTInAccount(accPub, signKP, "acl-user", pub, sub, exp)
	if err != nil {
		t.Fatalf("mint user: %v", err)
	}
	kp, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		t.Fatalf("user seed: %v", err)
	}
	nc, err := nats.Connect(url,
		nats.UserJWT(
			func() (string, error) { return userJWT, nil },
			func(nonce []byte) ([]byte, error) { return kp.Sign(nonce) },
		),
		nats.Timeout(10*time.Second),
		nats.MaxReconnects(0),
	)
	if err != nil {
		return len(userJWT), err
	}
	nc.Close()
	return len(userJWT), nil
}

// TestEmbeddedNATS_AcceptsLargeACLCredential is the production repro: a
// four-handle org's compiled credential must be able to connect.
func TestEmbeddedNATS_AcceptsLargeACLCredential(t *testing.T) {
	url, accPub, signKP, stop := startEmbeddedForTest(t, EmbeddedConfig{Port: 0})
	defer stop()

	const accountID = "f7ad9044-831d-498d-b83b-12898fd1388e"
	pub, sub := aclShapedSubjects(accountID, []string{"ashley", "barry", "father", "james"})

	size, err := connectWithSubjects(t, url, accPub, signKP, pub, sub)
	if size <= 4096 {
		t.Fatalf("test is not exercising the limit: credential is only %d bytes, need >4096", size)
	}
	if err != nil {
		t.Fatalf("a %d-byte ACL credential must connect, got: %v\n"+
			"the embedded server's MaxControlLine must exceed the compiled credential size",
			size, err)
	}
}

// TestEmbeddedNATS_ControlLineIsConfigurable proves the option actually
// reaches the server rather than being silently dropped.
//
// The credential here is deliberately sized into the band BETWEEN the
// configured 1024 and the nats-server stock 4096: an ignored option
// leaves the stock limit in force, which would accept this connection.
// Only a genuinely wired MaxControlLine rejects it.
func TestEmbeddedNATS_ControlLineIsConfigurable(t *testing.T) {
	url, accPub, signKP, stop := startEmbeddedForTest(t, EmbeddedConfig{Port: 0, MaxControlLine: 1024})
	defer stop()

	const accountID = "f7ad9044-831d-498d-b83b-12898fd1388e"
	pub, sub := aclShapedSubjects(accountID, []string{"ashley"})

	size, err := connectWithSubjects(t, url, accPub, signKP, pub, sub)
	if size <= 1024 || size >= 4096 {
		t.Fatalf("credential is %d bytes; test needs one between 1024 and the stock 4096 to discriminate", size)
	}
	if err == nil {
		t.Fatalf("MaxControlLine=1024 must reject a %d-byte credential; the option is not wired to the server", size)
	}
}

// TestDefaultMaxControlLine_HasHeadroom pins the shipped default against
// the production credential size plus room to grow.
func TestDefaultMaxControlLine_HasHeadroom(t *testing.T) {
	const observedProductionJWT = 4493
	if DefaultMaxControlLine <= observedProductionJWT {
		t.Fatalf("DefaultMaxControlLine=%d does not clear the observed production credential (%d bytes)",
			DefaultMaxControlLine, observedProductionJWT)
	}
	if DefaultMaxControlLine < 4*observedProductionJWT {
		t.Fatalf("DefaultMaxControlLine=%d leaves too little growth headroom; ACL credentials grow ~880 bytes per handle",
			DefaultMaxControlLine)
	}
}
