package acl

import "strings"

// Selector names the pipes a grant applies to. It matches a pipe's
// SUBJECT PATH — what natsubj.BuildSubject produces, minus the account
// prefix.
//
//	alice.inbox   exactly that pipe
//	alice.*       one token — every pipe directly under alice
//	alice.**      one or more trailing tokens — everything under alice
//	*.inbox       every handle's inbox
//	**            every pipe in the account
//
// `*` and `**` map 1:1 onto NATS `*` and `>`, which is what keeps the
// Phase 3 credential compiler close to mechanical.
//
// The manifold/collar ambiguity is inherited deliberately: natsubj
// already documents that `acct.X.pipe` is indistinguishable at the wire
// level between a manifold and a source segment. Matching the path
// inherits exactly that and introduces nothing new.
type Selector string

const (
	tokenAny  = "*"
	tokenRest = "**"
)

// Match reports whether sel covers path.
//
// Malformed selectors match nothing. That direction matters: a bad row
// in acl_grants must grant nothing, never the whole account.
func Match(sel Selector, path string) bool {
	if sel == "" || path == "" {
		return false
	}
	selParts := strings.Split(string(sel), ".")
	pathParts := strings.Split(path, ".")
	if hasEmpty(selParts) || hasEmpty(pathParts) {
		return false
	}
	// `**` is legal only as the final segment; anywhere else the
	// selector is malformed and matches nothing.
	for i, s := range selParts {
		if s == tokenRest && i != len(selParts)-1 {
			return false
		}
	}
	return matchParts(selParts, pathParts)
}

func matchParts(sel, path []string) bool {
	for i, s := range sel {
		if s == tokenRest {
			// One or more remaining tokens, matching NATS `>`. A grant
			// on "alice.**" is about what lives under alice, not alice
			// itself.
			return len(path) > i
		}
		if i >= len(path) {
			return false
		}
		if s != tokenAny && s != path[i] {
			return false
		}
	}
	// No `**`: every token had to line up exactly, so the lengths must
	// agree — `*` never spans a dot.
	return len(sel) == len(path)
}

func hasEmpty(parts []string) bool {
	for _, p := range parts {
		if p == "" {
			return true
		}
	}
	return false
}

// ValidSelector reports whether sel is well-formed. Storage rejects
// anything else: a malformed row would match nothing (Match fails
// closed), which is safe but silently useless — better to refuse it at
// the point someone types it.
func ValidSelector(sel Selector) bool {
	if sel == "" {
		return false
	}
	parts := strings.Split(string(sel), ".")
	for i, p := range parts {
		if p == "" {
			return false
		}
		if p == tokenRest && i != len(parts)-1 {
			return false
		}
	}
	return true
}
