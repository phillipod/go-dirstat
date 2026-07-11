// Package fsops applies guarded, auditable filesystem mutation plans.
package fsops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

const (
	MaxPlanBytes            int64 = 64 << 20
	MaxPlanOperations             = 100_000
	legacyPlanVersion             = 1
	PlanVersion                   = 2
	planRecordType                = "plan"
	operationRecordType           = "operation"
	resultRecordType              = "result"
	objectKindDirectory           = "directory"
	objectKindFile                = "file"
	objectKindSymlink             = "symlink"
	ResultStatusOK                = "ok"
	ResultStatusError             = "error"
	ResultStatusPartial           = "partial"
	ResultStatusIntent            = "intent"
	AuditPhaseIntent              = "intent"
	AuditPhaseOutcome             = "outcome"
	AuditStatusDisabled           = "disabled"
	AuditStatusNotAttempted       = "not_attempted"
	AuditStatusWritten            = "written"
	AuditStatusDurable            = "durable"
	AuditStatusFailed             = "failed"
)

type Action string

const (
	ActionDelete   Action = "delete"
	ActionCopy     Action = "copy"
	ActionMove     Action = "move"
	ActionRename   Action = "rename"
	ActionMkdir    Action = "mkdir"
	ActionTouch    Action = "touch"
	ActionTruncate Action = "truncate"
	ActionChmod    Action = "chmod"
	ActionChown    Action = "chown"
	ActionArchive  Action = "archive"
	ActionExtract  Action = "extract"
)

type PlanHeader struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	Root      string    `json:"root"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type Operation struct {
	Type        string        `json:"type,omitempty"`
	ID          string        `json:"id"`
	Action      Action        `json:"action"`
	Source      string        `json:"source,omitempty"`
	Destination string        `json:"destination,omitempty"`
	Expected    *fsinfo.Entry `json:"expected,omitempty"`
	// ExpectedDestination guards both reviewed absence and the identity of an
	// existing overwrite target. It was added in plan version 2.
	ExpectedDestination *fsinfo.PathExpectation `json:"expected_destination,omitempty"`
	Mode                *uint32                 `json:"mode,omitempty"`
	UID                 *int                    `json:"uid,omitempty"`
	GID                 *int                    `json:"gid,omitempty"`
	Size                *int64                  `json:"size,omitempty"`
	Format              string                  `json:"format,omitempty"`
	Recursive           bool                    `json:"recursive,omitempty"`
}

// OperationRequest is the unguarded, script-facing input used to construct a
// plan. IDs and source/destination expectations are deliberately absent: the
// planner assigns deterministic IDs and captures current guards only after the
// complete request set has passed static validation.
type OperationRequest struct {
	Action      Action  `json:"action"`
	Source      string  `json:"source"`
	Destination string  `json:"destination,omitempty"`
	Mode        *uint32 `json:"mode,omitempty"`
	UID         *int    `json:"uid,omitempty"`
	GID         *int    `json:"gid,omitempty"`
	Size        *int64  `json:"size,omitempty"`
	Format      string  `json:"format,omitempty"`
	Recursive   bool    `json:"recursive,omitempty"`
}

type Plan struct {
	Header     PlanHeader  `json:"header"`
	Operations []Operation `json:"operations"`
}

type Result struct {
	Type        string `json:"type"`
	Version     int    `json:"version"`
	OperationID string `json:"operation_id"`
	Action      Action `json:"action"`
	Status      string `json:"status"`
	DryRun      bool   `json:"dry_run,omitempty"`
	// MayHaveMutated is true for an explicit partial outcome: the operation
	// published useful state but could not finish every postcondition.
	MayHaveMutated bool `json:"may_have_mutated,omitempty"`
	// MutationCompleted distinguishes a completed filesystem operation from a
	// partial mutation when recording its outcome fails. A completed mutation
	// can therefore have status=partial when audit durability is uncertain.
	MutationCompleted bool `json:"mutation_completed,omitempty"`
	// AuditIntentStatus describes the pre-mutation intent record. AuditStatus
	// describes the post-operation outcome record.
	AuditIntentStatus string `json:"audit_intent_status,omitempty"`
	AuditStatus       string `json:"audit_status,omitempty"`
	// AuditPhase is present in audit-log records. Apply's returned operation
	// results omit it.
	AuditPhase string    `json:"audit_phase,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

type ConflictPolicy string

const (
	ConflictFail      ConflictPolicy = "fail"
	ConflictOverwrite ConflictPolicy = "overwrite"
)

type ApplyOptions struct {
	DryRun       bool
	Conflict     ConflictPolicy
	Audit        io.Writer
	AuditPath    string
	DisableAudit bool
	// AllowUnguarded is intended only for plan construction preflight. Normal
	// apply surfaces must require Expected metadata for every existing source.
	AllowUnguarded bool
	// filesystem is a narrow package-test seam for exercising cross-device
	// publication, copying, cleanup, sync, and close failures without depending
	// on real mounts.
	filesystem *mutationFilesystem
	// auditFactory is a package-test seam for write, sync, and close failures.
	// Callers use Audit or AuditPath instead.
	auditFactory func(string) (auditLog, error)
}

func supportedVersion(version int) bool {
	return version == legacyPlanVersion || version == PlanVersion
}

func WritePlan(w io.Writer, plan Plan) error {
	enc := json.NewEncoder(w)
	h := plan.Header
	h.Type = planRecordType
	if h.Version == 0 {
		h.Version = PlanVersion
	}
	if err := enc.Encode(h); err != nil {
		return fmt.Errorf("write plan header: %w", err)
	}
	for _, op := range plan.Operations {
		op.Type = operationRecordType
		if err := enc.Encode(op); err != nil {
			return fmt.Errorf("write operation %q: %w", op.ID, err)
		}
	}
	return nil
}

func ReadPlan(r io.Reader) (Plan, error) {
	return readPlanJSONL(r)
}

func WriteResult(w io.Writer, result Result) error {
	result.Type = resultRecordType
	if result.Version == 0 {
		result.Version = PlanVersion
	}
	return json.NewEncoder(w).Encode(result)
}

func WriteResults(w io.Writer, results []Result) error {
	for _, result := range results {
		if err := WriteResult(w, result); err != nil {
			return err
		}
	}
	return nil
}

func ReadResults(r io.Reader) ([]Result, error) {
	dec := json.NewDecoder(r)
	var results []Result
	for {
		var result Result
		if err := dec.Decode(&result); err != nil {
			if err == io.EOF {
				return results, nil
			}
			return nil, fmt.Errorf("read result: %w", err)
		}
		if result.Type != resultRecordType || !supportedVersion(result.Version) {
			return nil, fmt.Errorf("unsupported result type=%q version=%d", result.Type, result.Version)
		}
		results = append(results, result)
	}
}

func Apply(ctx context.Context, plan Plan, opts ApplyOptions) ([]Result, error) {
	return applyPlan(ctx, plan, opts)
}
