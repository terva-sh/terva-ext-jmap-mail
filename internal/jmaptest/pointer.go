package jmaptest

import (
	"fmt"
	"strconv"
	"strings"
)

// prior is one already-produced method response, kept as generic JSON values
// for result-reference evaluation.
type prior struct {
	name   string
	callID string
	args   any
}

// resolveRefs resolves RFC 8620 §3.7 result references in place: every
// "#"-prefixed argument is replaced by the value at `path` in the referenced
// earlier response. Failures are method-level errors per the spec.
func resolveRefs(args map[string]any, priors []prior) *methodErr {
	for key, val := range args {
		if !strings.HasPrefix(key, "#") {
			continue
		}
		plain := strings.TrimPrefix(key, "#")
		// §3.7: both the normal and referenced form present → invalidArguments.
		if _, dup := args[plain]; dup {
			return &methodErr{Type: "invalidArguments", Description: fmt.Sprintf("both %q and %q supplied", plain, key)}
		}
		ref, ok := val.(map[string]any)
		if !ok {
			return &methodErr{Type: "invalidResultReference", Description: key + " is not a ResultReference object"}
		}
		resultOf, _ := ref["resultOf"].(string)
		name, _ := ref["name"].(string)
		path, _ := ref["path"].(string)

		// §3.7: find the first response whose call id matches resultOf; its
		// method name must then match `name`.
		var src *prior
		for i := range priors {
			if priors[i].callID == resultOf {
				src = &priors[i]
				break
			}
		}
		if src == nil {
			return &methodErr{Type: "invalidResultReference", Description: fmt.Sprintf("no prior response with call id %q", resultOf)}
		}
		if src.name != name {
			return &methodErr{Type: "invalidResultReference", Description: fmt.Sprintf("call %q is %q, reference expects %q", resultOf, src.name, name)}
		}
		resolved, err := evalPointer(src.args, path)
		if err != nil {
			return &methodErr{Type: "invalidResultReference", Description: err.Error()}
		}
		delete(args, key)
		args[plain] = resolved
	}
	return nil
}

// evalPointer evaluates the RFC 8620 §3.7 extended JSON pointer (RFC 6901
// plus the "*" array wildcard with flattening).
func evalPointer(v any, path string) (any, error) {
	if path == "" {
		return v, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("pointer %q must start with /", path)
	}
	segs := strings.Split(path[1:], "/")
	for i, seg := range segs {
		// RFC 6901 escapes: ~1 → "/", ~0 → "~" (in that order).
		seg = strings.ReplaceAll(seg, "~1", "/")
		segs[i] = strings.ReplaceAll(seg, "~0", "~")
	}
	return evalSegs(v, segs)
}

func evalSegs(v any, segs []string) (any, error) {
	if len(segs) == 0 {
		return v, nil
	}
	seg, rest := segs[0], segs[1:]
	switch t := v.(type) {
	case map[string]any:
		child, ok := t[seg]
		if !ok {
			return nil, fmt.Errorf("no member %q", seg)
		}
		return evalSegs(child, rest)
	case []any:
		if seg == "*" {
			// §3.7: apply the rest of the pointer to every item; if a result
			// is itself an array, flatten it into the output.
			out := []any{}
			for _, item := range t {
				r, err := evalSegs(item, rest)
				if err != nil {
					return nil, err
				}
				if arr, ok := r.([]any); ok {
					out = append(out, arr...)
				} else {
					out = append(out, r)
				}
			}
			return out, nil
		}
		idx, err := strconv.Atoi(seg)
		if err != nil || idx < 0 || idx >= len(t) {
			return nil, fmt.Errorf("bad array index %q", seg)
		}
		return evalSegs(t[idx], rest)
	default:
		return nil, fmt.Errorf("cannot descend into %T with segment %q", v, seg)
	}
}
