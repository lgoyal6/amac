package router

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractJSON pulls a JSON object out of a model response.
//
// Small models are markedly worse than frontier ones at returning bare JSON:
// they wrap it in prose, or in a fenced code block, or both. Being strict here
// would escalate on presentation rather than on comprehension, which would
// destroy the savings for no quality gain. Being lenient about the wrapper and
// strict about the content is the right trade.
func ExtractJSON(s string) (map[string]any, error) {
	s = strings.TrimSpace(s)

	if obj, err := decode(s); err == nil {
		return obj, nil
	}

	// Fenced block, with or without a language tag.
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			if obj, err := decode(strings.TrimSpace(rest[:j])); err == nil {
				return obj, nil
			}
		}
	}

	// Last resort: the outermost brace pair. Scanning for balance rather than
	// taking the first and last brace, so prose containing a stray brace does
	// not break an otherwise valid answer.
	if start := strings.IndexByte(s, '{'); start >= 0 {
		depth, inStr, esc := 0, false, false
		for i := start; i < len(s); i++ {
			c := s[i]
			switch {
			case esc:
				esc = false
			case c == '\\' && inStr:
				esc = true
			case c == '"':
				inStr = !inStr
			case inStr:
			case c == '{':
				depth++
			case c == '}':
				depth--
				if depth == 0 {
					if obj, err := decode(s[start : i+1]); err == nil {
						return obj, nil
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("no JSON object found in %q", truncate(s, 120))
}

func decode(s string) (map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, fmt.Errorf("null object")
	}
	return obj, nil
}
