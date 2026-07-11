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

const PlanVersion = 1

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
	Mode        *uint32       `json:"mode,omitempty"`
	UID         *int          `json:"uid,omitempty"`
	GID         *int          `json:"gid,omitempty"`
	Size        *int64        `json:"size,omitempty"`
	Format      string        `json:"format,omitempty"`
	Recursive   bool          `json:"recursive,omitempty"`
}

type Plan struct {
	Header     PlanHeader  `json:"header"`
	Operations []Operation `json:"operations"`
}

type Result struct {
	Type        string    `json:"type"`
	Version     int       `json:"version"`
	OperationID string    `json:"operation_id"`
	Action      Action    `json:"action"`
	Status      string    `json:"status"`
	DryRun      bool      `json:"dry_run,omitempty"`
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
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
}

func WritePlan(w io.Writer, plan Plan) error {
	enc := json.NewEncoder(w)
	h := plan.Header
	h.Type = "plan"
	if h.Version == 0 {
		h.Version = PlanVersion
	}
	if err := enc.Encode(h); err != nil {
		return fmt.Errorf("write plan header: %w", err)
	}
	for _, op := range plan.Operations {
		op.Type = "operation"
		if err := enc.Encode(op); err != nil {
			return fmt.Errorf("write operation %q: %w", op.ID, err)
		}
	}
	return nil
}

func ReadPlan(r io.Reader) (Plan, error) {
	dec := json.NewDecoder(r)
	var p Plan
	if err := dec.Decode(&p.Header); err != nil {
		return Plan{}, fmt.Errorf("read plan header: %w", err)
	}
	if p.Header.Type != "plan" || p.Header.Version != PlanVersion {
		return Plan{}, fmt.Errorf("unsupported plan header type=%q version=%d", p.Header.Type, p.Header.Version)
	}
	for {
		var op Operation
		if err := dec.Decode(&op); err != nil {
			if err == io.EOF {
				break
			}
			return Plan{}, fmt.Errorf("read operation: %w", err)
		}
		if op.Type != "operation" {
			return Plan{}, fmt.Errorf("unexpected plan record type %q", op.Type)
		}
		p.Operations = append(p.Operations, op)
	}
	return p, nil
}

func WriteResult(w io.Writer, result Result) error {
	result.Type = "result"
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
		if result.Type != "result" || result.Version != PlanVersion {
			return nil, fmt.Errorf("unsupported result type=%q version=%d", result.Type, result.Version)
		}
		results = append(results, result)
	}
}

func Apply(ctx context.Context, plan Plan, opts ApplyOptions) ([]Result, error) {
	return apply(ctx, plan, opts)
}
