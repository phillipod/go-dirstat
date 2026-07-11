package fsops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

type planEffectKind uint8

const (
	planEffectProduce planEffectKind = iota
	planEffectRemove
	planEffectMutate
)

type planEffect struct {
	path         string
	kind         planEffectKind
	objectKind   string
	archiveValid bool
	tree         bool
	parentChange bool
	operationID  string
}

type planPathState struct {
	exists       bool
	objectKind   string
	fromPlan     bool
	changed      bool
	archiveValid bool
	operationID  string
}

type planValidator struct {
	root      string
	policy    ConflictPolicy
	unguarded bool
	effects   []planEffect
	initial   map[string]planPathState
}

type validatedPlanOperation struct {
	index     int
	operation Operation
	dependent bool
}

type planPrevalidationError struct {
	index     int
	operation Operation
	cause     error
}

// ValidateOperationRequest checks action and field compatibility without
// reading filesystem state. Plan builders use it before deduplication so an
// invalid duplicate cannot be silently hidden by normalization.
func ValidateOperationRequest(request OperationRequest) error {
	operation := Operation{
		Action: request.Action, Source: request.Source, Destination: request.Destination,
		Mode: request.Mode, UID: request.UID, GID: request.GID, Size: request.Size,
		Format: request.Format, Recursive: request.Recursive,
	}
	if err := validateAction(operation.Action); err != nil {
		return err
	}
	if err := validateOperationFields(operation); err != nil {
		return err
	}
	return validateParameters(operation, operation.Source, operation.Destination)
}

// ResolvePlanPath canonicalizes a request path without following its final
// component and proves that the resolved parent chain remains inside root.
// Plan builders must call this before inspecting request metadata.
func ResolvePlanPath(root, path string) (string, error) {
	return validationPath(root, path)
}

func (e *planPrevalidationError) Error() string {
	return fmt.Sprintf("operation %d (%q): %v", e.index+1, e.operation.ID, e.cause)
}

func (e *planPrevalidationError) Unwrap() error { return e.cause }

func prevalidatePlan(
	ctx context.Context,
	root string,
	plan Plan,
	policy ConflictPolicy,
	allowUnguarded bool,
	filesystem mutationFilesystem,
) error {
	if len(plan.Operations) == 0 {
		return errors.New("plan contains no operations")
	}
	validator := planValidator{
		root: root, policy: policy, unguarded: allowUnguarded,
		initial: make(map[string]planPathState),
	}
	validated := make([]validatedPlanOperation, 0, len(plan.Operations))
	seenIDs := make(map[string]bool, len(plan.Operations))
	for index, operation := range plan.Operations {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, err := validator.validateOperation(plan.Header.Version, operation, seenIDs)
		if err != nil {
			return &planPrevalidationError{index: index, operation: operation, cause: err}
		}
		item.index = index
		validated = append(validated, item)
	}
	for _, item := range validated {
		if item.dependent {
			continue
		}
		if err := execute(ctx, root, item.operation, true, policy, allowUnguarded, filesystem); err != nil {
			return &planPrevalidationError{
				index: item.index, operation: item.operation,
				cause: fmt.Errorf("preflight: %w", err),
			}
		}
	}
	return nil
}

func (v *planValidator) validateOperation(
	planVersion int,
	operation Operation,
	seenIDs map[string]bool,
) (validatedPlanOperation, error) {
	if strings.TrimSpace(operation.ID) == "" {
		return validatedPlanOperation{}, errors.New("operation ID is required")
	}
	if seenIDs[operation.ID] {
		return validatedPlanOperation{}, fmt.Errorf("duplicate operation ID %q", operation.ID)
	}
	seenIDs[operation.ID] = true
	if operation.Type != "" && operation.Type != operationRecordType {
		return validatedPlanOperation{}, fmt.Errorf("unexpected record type %q", operation.Type)
	}
	if err := validateAction(operation.Action); err != nil {
		return validatedPlanOperation{}, err
	}
	if planVersion == legacyPlanVersion && operation.ExpectedDestination != nil {
		return validatedPlanOperation{}, errors.New("plan version 1 cannot contain an expected destination guard")
	}

	source, err := validationPath(v.root, operation.Source)
	if err != nil {
		return validatedPlanOperation{}, fmt.Errorf("source: %w", err)
	}
	if samePath(source, v.root) &&
		(operation.Action == ActionDelete || operation.Action == ActionMove || operation.Action == ActionRename) {
		return validatedPlanOperation{}, fmt.Errorf("refusing to %s the plan root", operation.Action)
	}
	if err := validateOperationFields(operation); err != nil {
		return validatedPlanOperation{}, err
	}
	if operation.Expected != nil && operation.Expected.Path != "" {
		expectedPath, pathErr := canonicalNoFollowPath(operation.Expected.Path)
		if pathErr != nil || !samePath(expectedPath, source) {
			return validatedPlanOperation{}, errors.New("stale source: expected path does not match operation source")
		}
	}

	destination := ""
	if operation.Destination != "" {
		destination, err = validationPath(v.root, operation.Destination)
		if err != nil {
			return validatedPlanOperation{}, fmt.Errorf("destination: %w", err)
		}
		if samePath(source, destination) {
			return validatedPlanOperation{}, errors.New("source and destination must differ")
		}
	}
	if operation.ExpectedDestination != nil {
		if operation.ExpectedDestination.Path == "" {
			return validatedPlanOperation{}, errors.New("invalid destination expectation: path is required")
		}
		expectedPath, pathErr := canonicalNoFollowPath(operation.ExpectedDestination.Path)
		if pathErr != nil || !samePath(expectedPath, destination) {
			return validatedPlanOperation{}, errors.New("stale destination: expected path does not match operation destination")
		}
	}
	if err := validateParameters(operation, source, destination); err != nil {
		return validatedPlanOperation{}, err
	}

	sourceState, err := v.pathState(source)
	if err != nil {
		return validatedPlanOperation{}, fmt.Errorf("inspect source: %w", err)
	}
	dependent := sourceState.changed
	if !actionAllowsMissingSource(operation.Action) && !sourceState.exists {
		if sourceState.operationID != "" {
			return validatedPlanOperation{}, fmt.Errorf(
				"invalid dependency: source %q was made unavailable by operation %q",
				source, sourceState.operationID,
			)
		}
		return validatedPlanOperation{}, fmt.Errorf("source %q does not exist", source)
	}
	if operation.Action == ActionMkdir && sourceState.exists {
		return validatedPlanOperation{}, fmt.Errorf("mkdir source %q already exists", source)
	}
	if sourceState.changed && operation.Expected != nil {
		return validatedPlanOperation{}, fmt.Errorf(
			"invalid dependency: source guard was captured before operation %q changed %q",
			sourceState.operationID, source,
		)
	}
	if sourceState.exists && operation.Expected == nil && !v.unguarded && !sourceState.fromPlan {
		return validatedPlanOperation{}, errors.New("existing source requires expected metadata guard")
	}
	if sourceState.fromPlan && operation.Expected != nil {
		return validatedPlanOperation{}, errors.New("invalid dependency: a source produced by this plan cannot use pre-plan expected metadata")
	}
	if operation.Action == ActionExtract && sourceState.fromPlan && !sourceState.archiveValid {
		return validatedPlanOperation{}, fmt.Errorf(
			"invalid dependency: extract source %q was produced by operation %q but cannot be validated before mutation",
			source, sourceState.operationID,
		)
	}

	if actionAllowsMissingSource(operation.Action) {
		parentState, parentErr := v.pathState(filepath.Dir(source))
		if parentErr != nil {
			return validatedPlanOperation{}, fmt.Errorf("inspect source parent: %w", parentErr)
		}
		if !parentState.exists || parentState.objectKind != objectKindDirectory {
			return validatedPlanOperation{}, errors.New("source parent is not an available directory")
		}
		dependent = dependent || parentState.fromPlan
	}

	if destination != "" {
		destinationState, stateErr := v.pathState(destination)
		if stateErr != nil {
			return validatedPlanOperation{}, fmt.Errorf("inspect destination: %w", stateErr)
		}
		if destinationState.changed {
			return validatedPlanOperation{}, fmt.Errorf(
				"invalid dependency: destination %q was changed by operation %q",
				destination, destinationState.operationID,
			)
		}
		parentState, parentErr := v.pathState(filepath.Dir(destination))
		if parentErr != nil {
			return validatedPlanOperation{}, fmt.Errorf("inspect destination parent: %w", parentErr)
		}
		if !parentState.exists || parentState.objectKind != objectKindDirectory {
			return validatedPlanOperation{}, errors.New("destination parent is not an available directory")
		}
		dependent = dependent || parentState.fromPlan
		if v.policy == ConflictFail && destinationState.exists {
			return validatedPlanOperation{}, fmt.Errorf("destination %q already exists", destination)
		}
		if v.policy == ConflictOverwrite && operation.ExpectedDestination == nil && !v.unguarded {
			return validatedPlanOperation{}, errors.New("overwrite destination requires expected destination guard")
		}
		if operation.ExpectedDestination != nil {
			if err := checkDestinationExpectation(*operation.ExpectedDestination, destination); err != nil {
				return validatedPlanOperation{}, err
			}
		}
		if sourceState.objectKind == objectKindDirectory && pathContains(source, destination) &&
			(operation.Action == ActionCopy || operation.Action == ActionMove || operation.Action == ActionRename || operation.Action == ActionArchive) {
			return validatedPlanOperation{}, fmt.Errorf("destination %q is inside source %q", destination, source)
		}
	}

	if err := validatePlannedSourceKind(operation, sourceState.objectKind); err != nil {
		return validatedPlanOperation{}, err
	}
	v.effects = append(v.effects, operationEffects(operation, source, destination, sourceState)...)
	return validatedPlanOperation{operation: operation, dependent: dependent}, nil
}

func validateOperationFields(operation Operation) error {
	hasDestination := operation.Action == ActionCopy || operation.Action == ActionMove || operation.Action == ActionRename ||
		operation.Action == ActionArchive || operation.Action == ActionExtract
	if hasDestination && strings.TrimSpace(operation.Destination) == "" {
		return errors.New("destination is required")
	}
	if !hasDestination && operation.Destination != "" {
		return fmt.Errorf("destination cannot be used with %s", operation.Action)
	}
	if operation.ExpectedDestination != nil && !hasDestination {
		return fmt.Errorf("expected destination cannot be used with %s", operation.Action)
	}
	if operation.Recursive && operation.Action != ActionDelete {
		return fmt.Errorf("recursive cannot be used with %s", operation.Action)
	}
	if operation.Mode != nil && operation.Action != ActionMkdir && operation.Action != ActionTouch && operation.Action != ActionChmod {
		return fmt.Errorf("mode cannot be used with %s", operation.Action)
	}
	if operation.Mode != nil && *operation.Mode > 0o7777 {
		return fmt.Errorf("invalid mode %#o", *operation.Mode)
	}
	if operation.Size != nil && operation.Action != ActionTruncate {
		return fmt.Errorf("size cannot be used with %s", operation.Action)
	}
	if (operation.UID != nil || operation.GID != nil) && operation.Action != ActionChown {
		return fmt.Errorf("uid or gid cannot be used with %s", operation.Action)
	}
	if operation.UID != nil && *operation.UID < 0 {
		return errors.New("uid must be zero or greater")
	}
	if operation.GID != nil && *operation.GID < 0 {
		return errors.New("gid must be zero or greater")
	}
	if operation.Format != "" && operation.Action != ActionArchive && operation.Action != ActionExtract {
		return fmt.Errorf("format cannot be used with %s", operation.Action)
	}
	return nil
}

func validationPath(root, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	canonical, err := canonicalNoFollowPath(path)
	if err != nil {
		return "", err
	}
	if !within(root, canonical) {
		return "", fmt.Errorf("resolved path %q escapes root %q", canonical, root)
	}
	return canonical, nil
}

func (v *planValidator) pathState(path string) (planPathState, error) {
	state, ok := v.initial[path]
	if !ok {
		entry, err := fsinfo.Inspect(path, false)
		switch {
		case err == nil:
			state = planPathState{exists: true, objectKind: entry.Kind}
		case errors.Is(err, fs.ErrNotExist):
			state = planPathState{}
		default:
			return planPathState{}, err
		}
		v.initial[path] = state
	}
	for _, effect := range v.effects {
		switch {
		case samePath(effect.path, path):
			state.changed, state.operationID = true, effect.operationID
			switch effect.kind {
			case planEffectProduce:
				state.exists, state.objectKind, state.fromPlan = true, effect.objectKind, true
				state.archiveValid = effect.archiveValid
			case planEffectRemove:
				state.exists, state.objectKind, state.fromPlan = false, "", false
			case planEffectMutate:
			}
		case effect.kind == planEffectRemove && effect.tree && pathContains(effect.path, path):
			state = planPathState{changed: true, operationID: effect.operationID}
		case effect.parentChange && pathContains(path, effect.path):
			state.changed, state.operationID = true, effect.operationID
		}
	}
	return state, nil
}

func operationEffects(operation Operation, source, destination string, sourceState planPathState) []planEffect {
	effects := make([]planEffect, 0, 2)
	removeSource := func() {
		effects = append(effects, planEffect{
			path: source, kind: planEffectRemove, tree: sourceState.objectKind == objectKindDirectory,
			parentChange: true, operationID: operation.ID,
		})
	}
	produce := func(path, objectKind string, archiveValid bool) {
		effects = append(effects, planEffect{
			path: path, kind: planEffectProduce, objectKind: objectKind, archiveValid: archiveValid,
			parentChange: true, operationID: operation.ID,
		})
	}
	mutate := func() {
		effects = append(effects, planEffect{path: source, kind: planEffectMutate, operationID: operation.ID})
	}
	switch operation.Action {
	case ActionDelete:
		removeSource()
	case ActionMove, ActionRename:
		removeSource()
		produce(destination, sourceState.objectKind, sourceState.archiveValid)
	case ActionCopy:
		produce(destination, sourceState.objectKind, sourceState.archiveValid)
	case ActionMkdir:
		produce(source, objectKindDirectory, false)
	case ActionTouch:
		if sourceState.exists {
			mutate()
		} else {
			produce(source, objectKindFile, false)
		}
	case ActionTruncate, ActionChmod, ActionChown:
		mutate()
	case ActionArchive:
		produce(destination, objectKindFile, true)
	case ActionExtract:
		produce(destination, objectKindDirectory, false)
	}
	return effects
}

func validatePlannedSourceKind(operation Operation, kind string) error {
	switch operation.Action {
	case ActionCopy:
		if kind != objectKindDirectory && kind != objectKindFile && kind != objectKindSymlink {
			return fmt.Errorf("cannot copy source kind %q", kind)
		}
	case ActionExtract:
		if kind != objectKindFile {
			return errors.New("extract source must be a regular file")
		}
	case ActionTouch, ActionTruncate, ActionChmod, ActionChown:
		if kind == objectKindSymlink {
			return errors.New("refusing to follow final symlink")
		}
	case ActionDelete, ActionMove, ActionRename, ActionMkdir, ActionArchive:
	}
	return nil
}

func pathContains(parent, child string) bool {
	if samePath(parent, child) {
		return false
	}
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
