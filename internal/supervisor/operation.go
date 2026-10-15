package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lgoyal6/amac/internal/acp"
	"github.com/lgoyal6/amac/internal/event"
)

// WriteScope is an explicit, one-session grant for one exact path. ACP's
// permission request does not identify a later fs/write_text_file request, so
// that permission response is deliberately not used as this grant.
type WriteScope struct {
	SessionID       string
	AuthorizationID string
	Path            string
	ExpiresAt       time.Time
}

// AuthorizeWrite installs an explicit grant. Callers must bind the grant to
// the current AMAC session; ordinary ask/auto/reject permission modes do not
// create filesystem authority.
func (s *Session) AuthorizeWrite(scope WriteScope) error {
	if scope.SessionID != s.ID || scope.AuthorizationID == "" {
		return fmt.Errorf("write authority identity mismatch")
	}
	if !filepath.IsAbs(scope.Path) || filepath.Clean(scope.Path) != scope.Path {
		return fmt.Errorf("write authority path must be an absolute clean path")
	}
	if !scope.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("write authority is expired")
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.Lock()
	s.writeScope = &scope
	s.mu.Unlock()
	return nil
}

// RevokeWrite removes the currently installed grant. It waits for an already
// admitted operation to finish, so revocation cannot race a file mutation.
func (s *Session) RevokeWrite(authorizationID string) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeScope == nil || s.writeScope.AuthorizationID != authorizationID {
		return fmt.Errorf("write authority %q is not current", authorizationID)
	}
	s.writeScope = nil
	return nil
}

type operationRecord struct {
	OperationID     string `json:"operationId"`
	SessionID       string `json:"sessionId"`
	ACPSessionID    string `json:"acpSessionId"`
	RequestID       string `json:"requestId"`
	AuthorizationID string `json:"authorizationId"`
	Scope           string `json:"scope"`
	Path            string `json:"path"`
	Outcome         string `json:"outcome,omitempty"`
	Error           string `json:"error,omitempty"`
	Bytes           int    `json:"bytes,omitempty"`
	InputDigest     string `json:"inputDigest"`
}

// WritePlan is a read-only rendering of the exact write that an authorized
// request would perform. Returning one never records an operation or touches
// the filesystem; execution remains a separate fs/write_text_file request.
type WritePlan struct {
	DryRun          bool               `json:"dryRun"`
	OperationID     string             `json:"operationId"`
	SessionID       string             `json:"sessionId"`
	ACPSessionID    string             `json:"acpSessionId"`
	RequestID       string             `json:"requestId"`
	AuthorizationID string             `json:"authorizationId"`
	Scope           string             `json:"scope"`
	Contract        WritePlanContract  `json:"contract"`
	ToolCall        PlannedToolCall    `json:"toolCall"`
	ExpectedEffect  PlannedWriteEffect `json:"expectedEffect"`
	ExpectedAudit   []string           `json:"expectedAuditEvents"`
}

type WritePlanContract struct {
	SessionID       string    `json:"sessionId"`
	AuthorizationID string    `json:"authorizationId"`
	ExactPath       string    `json:"exactPath"`
	ExpiresAt       time.Time `json:"expiresAt"`
	Scope           string    `json:"scope"`
}

type PlannedToolCall struct {
	Name   string                  `json:"name"`
	Params acp.WriteTextFileParams `json:"params"`
}

type PlannedWriteEffect struct {
	Kind                    string `json:"kind"`
	Path                    string `json:"path"`
	Bytes                   int    `json:"bytes"`
	InputDigest             string `json:"inputDigest"`
	CreateParentDirectories bool   `json:"createParentDirectories"`
	ParentDirectoryMode     string `json:"parentDirectoryMode"`
	FileMode                string `json:"fileMode"`
}

func operationID(sessionID string, requestID json.RawMessage) string {
	sum := sha256.Sum256(append(append([]byte(sessionID+"\x00"), requestID...), 0))
	return hex.EncodeToString(sum[:])
}

// authorizedWritePlan validates the same identity, exact-path grant, and
// live-owner boundary used by execution. Callers hold operationMu so grant
// revocation cannot race the check.
func (s *Session) authorizedWritePlan(ctx context.Context, req acp.IncomingRequest, p acp.WriteTextFileParams) (WritePlan, int, error) {
	if p.SessionID != s.ACPID || !filepath.IsAbs(p.Path) || filepath.Clean(p.Path) != p.Path {
		return WritePlan{}, -32602, fmt.Errorf("write session or path is invalid")
	}
	if len(req.RequestID) == 0 || string(req.RequestID) == "null" {
		return WritePlan{}, -32602, fmt.Errorf("write request has no stable request id")
	}
	s.mu.RLock()
	scope := s.writeScope
	if scope != nil {
		copy := *scope
		scope = &copy
	}
	s.mu.RUnlock()
	if !writeScopeAllows(scope, s.ID, p.Path, time.Now()) {
		return WritePlan{}, -32000, fmt.Errorf("no current write authority for this session and exact path")
	}
	if err := s.operationOwnerError(ctx); err != nil {
		return WritePlan{}, -32000, err
	}
	id := operationID(s.ID, req.RequestID)
	digest := inputDigest(p.Content)
	return WritePlan{
		DryRun: true, OperationID: id, SessionID: s.ID, ACPSessionID: s.ACPID,
		RequestID: string(req.RequestID), AuthorizationID: scope.AuthorizationID,
		Scope: "fs.write_text_file:exact-path",
		Contract: WritePlanContract{SessionID: s.ID, AuthorizationID: scope.AuthorizationID,
			ExactPath: scope.Path, ExpiresAt: scope.ExpiresAt, Scope: "fs.write_text_file:exact-path"},
		ToolCall: PlannedToolCall{Name: "fs/write_text_file", Params: p},
		ExpectedEffect: PlannedWriteEffect{Kind: "write_text_file", Path: p.Path, Bytes: len(p.Content), InputDigest: digest,
			CreateParentDirectories: true, ParentDirectoryMode: "0755", FileMode: "0644"},
		ExpectedAudit: []string{"operation.intent", "operation.effect", "actuation"},
	}, 0, nil
}

func (s *Session) handleWritePreview(ctx context.Context, req acp.IncomingRequest, p acp.WriteTextFileParams) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	plan, code, err := s.authorizedWritePlan(ctx, req, p)
	if err != nil {
		_ = req.RespondErr(code, err.Error())
		return
	}
	prior, err := s.operationEvents(ctx, plan.OperationID)
	if err != nil {
		_ = req.RespondErr(-32000, "cannot check operation history: "+err.Error())
		return
	}
	if prior.intent != nil || prior.effect != nil {
		_ = req.RespondErr(-32000, "operation id already exists in durable history")
		return
	}
	_ = req.Respond(plan)
}

func (s *Session) handleWrite(ctx context.Context, req acp.IncomingRequest, p acp.WriteTextFileParams) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	plan, code, err := s.authorizedWritePlan(ctx, req, p)
	if err != nil {
		_ = req.RespondErr(code, err.Error())
		return
	}
	opID := plan.OperationID
	prior, err := s.operationEvents(ctx, opID)
	if err != nil {
		_ = req.RespondErr(-32000, "cannot check operation history: "+err.Error())
		return
	}
	if prior.effect != nil && prior.intent == nil {
		_ = req.RespondErr(-32000, "operation receipt has no matching durable intent")
		return
	}
	previous := prior.intent
	if prior.effect != nil {
		previous = prior.effect
	}
	if previous != nil {
		var old operationRecord
		if err := json.Unmarshal(previous.Payload, &old); err != nil ||
			old.OperationID != opID || old.SessionID != s.ID || old.ACPSessionID != s.ACPID ||
			old.RequestID != string(req.RequestID) || old.AuthorizationID != plan.AuthorizationID ||
			old.Scope != "fs.write_text_file:exact-path" || old.Path != p.Path ||
			old.InputDigest != inputDigest(p.Content) {
			_ = req.RespondErr(-32000, "operation identity or authority does not match its durable record")
			return
		}
	}
	if prior.effect != nil {
		var rec operationRecord
		if err := json.Unmarshal(prior.effect.Payload, &rec); err != nil {
			_ = req.RespondErr(-32000, "operation receipt is unreadable")
			return
		}
		if rec.Outcome == "succeeded" {
			_ = req.Respond(nil)
			return
		}
		_ = req.RespondErr(-32000, "operation already completed with failure: "+rec.Error)
		return
	}
	if prior.intent != nil {
		_ = req.RespondErr(-32000, "operation outcome is uncertain; refusing duplicate delivery")
		return
	}

	rec := operationRecord{
		OperationID: opID, SessionID: s.ID, ACPSessionID: s.ACPID,
		RequestID: string(req.RequestID), AuthorizationID: plan.AuthorizationID,
		Scope: plan.Scope, Path: p.Path, InputDigest: plan.ExpectedEffect.InputDigest,
	}
	if err := s.recordRequired(ctx, event.KindOperationIntent, rec); err != nil {
		_ = req.RespondErr(-32000, "cannot persist operation intent: "+err.Error())
		return
	}

	writeErr := s.operationOwnerError(ctx)
	if writeErr == nil {
		writeErr = os.MkdirAll(filepath.Dir(p.Path), 0o755)
	}
	if writeErr == nil {
		writeErr = os.WriteFile(p.Path, []byte(p.Content), 0o644)
	}
	if writeErr != nil {
		rec.Outcome, rec.Error = "failed", writeErr.Error()
	} else {
		rec.Outcome, rec.Bytes = "succeeded", len(p.Content)
	}
	if err := s.recordRequired(ctx, event.KindOperationEffect, rec); err != nil {
		_ = req.RespondErr(-32000, "effect occurred or failed, but receipt could not be persisted; refusing retry")
		return
	}
	if writeErr != nil {
		_ = req.RespondErr(-32000, writeErr.Error())
		return
	}
	s.sup.record(event.KindActuation, s.ID, map[string]any{"op": "write", "path": p.Path, "bytes": len(p.Content), "operationId": opID})
	_ = req.Respond(nil)
}

func (s *Session) operationOwnerError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("operation owner context ended: %w", err)
	}
	select {
	case <-s.client.Done():
		return fmt.Errorf("operation owner process exited")
	default:
		return nil
	}
}

func writeScopeAllows(scope *WriteScope, sessionID, target string, now time.Time) bool {
	return scope != nil && scope.SessionID == sessionID && scope.Path == target &&
		scope.AuthorizationID != "" && scope.ExpiresAt.After(now)
}

func inputDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (s *Session) recordRequired(ctx context.Context, kind event.Kind, payload any) error {
	ev, err := event.New(kind, "supervisor", s.ID, payload)
	if err != nil {
		return err
	}
	_, err = s.sup.log.Append(ctx, ev)
	return err
}

func (s *Session) operationEvents(ctx context.Context, id string) (struct {
	intent *event.Event
	effect *event.Event
}, error) {
	var out struct {
		intent *event.Event
		effect *event.Event
	}
	rows, err := s.sup.log.OperationEvents(ctx, id)
	if err != nil {
		return out, err
	}
	for i := range rows {
		copy := rows[i]
		if copy.Kind == event.KindOperationIntent {
			out.intent = &copy
		} else if copy.Kind == event.KindOperationEffect {
			out.effect = &copy
		}
	}
	return out, nil
}
