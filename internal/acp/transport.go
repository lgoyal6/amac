package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// ACP frames messages as newline-delimited JSON over stdio. That is not the
// same as LSP, which prefixes every message with a Content-Length header. We
// verified the framing against the live claude-agent-acp and codex-acp
// adapters rather than inferring it from the spec.
type transport struct {
	r *bufio.Scanner
	w io.Writer

	// One writer, many goroutines: requests are issued from callers while the
	// read loop answers agent-initiated requests on the same pipe.
	mu sync.Mutex
}

// A single agent message routinely exceeds bufio.Scanner's 64KB default: a
// tool result carrying a file, or a long assistant turn, will silently fail to
// scan and look exactly like the agent went quiet. Give it room and treat
// overflow as the hard error it is.
const maxMessageBytes = 32 * 1024 * 1024

func newTransport(r io.Reader, w io.Writer) *transport {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxMessageBytes)
	return &transport{r: sc, w: w}
}

func (t *transport) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal outgoing: %w", err)
	}
	b = append(b, '\n')

	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.w.Write(b); err != nil {
		return fmt.Errorf("write to agent: %w", err)
	}
	return nil
}

// recv returns the next raw message. Blank lines are skipped: some adapters
// emit them around startup banners.
func (t *transport) recv() (json.RawMessage, error) {
	for t.r.Scan() {
		line := t.r.Bytes()
		if len(trimSpace(line)) == 0 {
			continue
		}
		// Scanner reuses its buffer, so the caller gets a copy or it will be
		// overwritten under them on the next Scan.
		out := make(json.RawMessage, len(line))
		copy(out, line)
		return out, nil
	}
	if err := t.r.Err(); err != nil {
		return nil, fmt.Errorf("read from agent: %w", err)
	}
	return nil, io.EOF
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
