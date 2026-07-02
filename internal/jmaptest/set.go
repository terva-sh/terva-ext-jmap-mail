package jmaptest

import (
	"fmt"
	"strings"
)

// maxObjectsInSet mirrors the limit the session advertises.
const maxObjectsInSet = 500

func setError(typ, desc string) map[string]any {
	m := map[string]any{"type": typ}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

// emailSet implements Email/set (RFC 8620 §5.3, RFC 8621 §4.6): update with
// JMAP patch syntax (whole-property replace plus keywords/<kw> and
// mailboxIds/<id> pointers) and destroy. Creation is not supported (nothing
// in this extension creates mail). Each object's patch applies atomically:
// any invalid part fails the whole object into notUpdated.
func (s *Server) emailSet(args map[string]any) (any, *methodErr) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// RFC 8620 §5.3 ifInState → stateMismatch.
	if v, present := args["ifInState"]; present && v != nil {
		if str, _ := v.(string); str != s.stateLocked() {
			return nil, &methodErr{Type: "stateMismatch", Description: "ifInState does not match the current state"}
		}
	}
	if create, _ := args["create"].(map[string]any); len(create) > 0 {
		return nil, &methodErr{Type: "forbidden", Description: "Email/set create is not supported by this fake"}
	}
	update, _ := args["update"].(map[string]any)
	destroy := stringSlice(args["destroy"])
	if len(update)+len(destroy) > maxObjectsInSet {
		return nil, &methodErr{Type: "requestTooLarge", Description: "more objects than maxObjectsInSet"}
	}

	oldState := s.stateLocked()
	byID := map[string]*Email{}
	for _, e := range s.emails {
		byID[e.ID] = e
	}

	updated := map[string]any{}
	notUpdated := map[string]any{}
	changed := false
	for id, patchAny := range update {
		e, ok := byID[id]
		if !ok {
			notUpdated[id] = setError("notFound", "")
			continue
		}
		patch, ok := patchAny.(map[string]any)
		if !ok {
			notUpdated[id] = setError("invalidPatch", "update value must be a patch object")
			continue
		}
		keywords, mailboxes, serr := s.applyPatchLocked(e, patch)
		if serr != nil {
			notUpdated[id] = serr
			continue
		}
		e.Keywords, e.MailboxIDs = keywords, mailboxes
		updated[id] = nil
		changed = true
	}

	destroyed := []string{}
	notDestroyed := map[string]any{}
	for _, id := range destroy {
		if _, ok := byID[id]; !ok {
			notDestroyed[id] = setError("notFound", "")
			continue
		}
		kept := s.emails[:0]
		for _, e := range s.emails {
			if e.ID != id {
				kept = append(kept, e)
			}
		}
		s.emails = kept
		destroyed = append(destroyed, id)
		changed = true
	}

	if changed {
		s.stateN++
	}
	return map[string]any{
		"accountId":    s.AccountID,
		"oldState":     oldState,
		"newState":     s.stateLocked(),
		"created":      map[string]any{},
		"updated":      updated,
		"notUpdated":   notUpdated,
		"destroyed":    destroyed,
		"notDestroyed": notDestroyed,
	}, nil
}

// applyPatchLocked computes the patched keywords/mailboxIds without mutating
// the message, so a partially-invalid patch changes nothing (RFC 8620 §5.3:
// "the whole update MUST fail").
func (s *Server) applyPatchLocked(e *Email, patch map[string]any) (keywords, mailboxes []string, serr map[string]any) {
	keywords = append([]string{}, e.Keywords...)
	mailboxes = append([]string{}, e.MailboxIDs...)
	for key, val := range patch {
		switch {
		case key == "keywords":
			set, ok := trueKeys(val)
			if !ok {
				return nil, nil, setError("invalidProperties", "keywords must map keyword → true")
			}
			keywords = set
		case strings.HasPrefix(key, "keywords/"):
			kw := strings.TrimPrefix(key, "keywords/")
			switch val {
			case true:
				keywords = addStr(keywords, kw)
			case nil:
				keywords = removeStr(keywords, kw)
			default:
				return nil, nil, setError("invalidProperties", key+" must be true or null")
			}
		case key == "mailboxIds":
			set, ok := trueKeys(val)
			if !ok {
				return nil, nil, setError("invalidProperties", "mailboxIds must map mailbox id → true")
			}
			for _, id := range set {
				if !s.mailboxExistsLocked(id) {
					return nil, nil, setError("invalidProperties", fmt.Sprintf("no mailbox %q", id))
				}
			}
			mailboxes = set
		case strings.HasPrefix(key, "mailboxIds/"):
			id := strings.TrimPrefix(key, "mailboxIds/")
			switch val {
			case true:
				if !s.mailboxExistsLocked(id) {
					return nil, nil, setError("invalidProperties", fmt.Sprintf("no mailbox %q", id))
				}
				mailboxes = addStr(mailboxes, id)
			case nil:
				mailboxes = removeStr(mailboxes, id)
			default:
				return nil, nil, setError("invalidProperties", key+" must be true or null")
			}
		default:
			return nil, nil, setError("invalidProperties", "unsupported update property "+key)
		}
	}
	// RFC 8621 §1/§4.6: a message always belongs to at least one mailbox.
	if len(mailboxes) == 0 {
		return nil, nil, setError("invalidProperties", "an email must belong to at least one mailbox")
	}
	return keywords, mailboxes, nil
}

// trueKeys extracts the keys of a JMAP id/keyword set map, requiring every
// value to be literal true (the only value the spec allows).
func trueKeys(v any) ([]string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(m))
	for k, val := range m {
		if val != true {
			return nil, false
		}
		out = append(out, k)
	}
	return out, true
}

func addStr(list []string, s string) []string {
	if containsStr(list, s) {
		return list
	}
	return append(list, s)
}

func removeStr(list []string, s string) []string {
	out := list[:0]
	for _, item := range list {
		if item != s {
			out = append(out, item)
		}
	}
	return out
}
