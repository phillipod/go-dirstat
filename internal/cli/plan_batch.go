package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
)

type operationPlanBuild struct {
	plan    fsops.Plan
	summary planSummary
}

type planSummary struct {
	Type                    string         `json:"type"`
	Version                 int            `json:"version"`
	InputOperations         int            `json:"input_operations"`
	Operations              int            `json:"operations"`
	DeduplicatedOperations  int            `json:"deduplicated_operations"`
	ActionCounts            map[string]int `json:"action_counts"`
	DeleteReclaimBytes      int64          `json:"delete_reclaim_estimate_bytes"`
	ReclaimEstimateComplete bool           `json:"reclaim_estimate_complete"`
	ReclaimEstimateErrors   int            `json:"reclaim_estimate_errors,omitempty"`
}

type commandContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader commandContextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := reader.reader.Read(buffer)
	if contextErr := reader.ctx.Err(); contextErr != nil {
		return count, contextErr
	}
	return count, err
}

func buildOperationPlanDetailed(cmd *cobra.Command, flags planFlags, args []string) (operationPlanBuild, error) {
	requests, err := collectOperationRequests(cmd, flags, args)
	if err != nil {
		return operationPlanBuild{}, err
	}
	if err := cmd.Context().Err(); err != nil {
		return operationPlanBuild{}, err
	}
	root, err := canonicalOperationRoot(flags.root)
	if err != nil {
		return operationPlanBuild{}, err
	}
	operations := make([]fsops.Operation, 0, len(requests))
	for index, request := range requests {
		if err := cmd.Context().Err(); err != nil {
			return operationPlanBuild{}, err
		}
		operation, operationErr := operationFromRequest(root, request)
		if operationErr != nil {
			return operationPlanBuild{}, fmt.Errorf("operation request %d: %w", index+1, operationErr)
		}
		operations = append(operations, operation)
	}
	inputCount := len(operations)
	operations, err = normalizePlanOperations(cmd.Context(), operations)
	if err != nil {
		return operationPlanBuild{}, err
	}
	if len(operations) == 0 {
		return operationPlanBuild{}, errors.New("plan contains no operations after normalization")
	}
	if len(operations) > fsops.MaxPlanOperations {
		return operationPlanBuild{}, fmt.Errorf("plan exceeds maximum of %d operations", fsops.MaxPlanOperations)
	}
	for index := range operations {
		operations[index].ID = fmt.Sprintf("%s-%d", operations[index].Action, index+1)
	}
	plan := fsops.Plan{
		Header:     fsops.PlanHeader{Version: fsops.PlanVersion, Root: root, CreatedAt: nowUTC()},
		Operations: operations,
	}
	if _, err := fsops.Apply(cmd.Context(), plan, fsops.ApplyOptions{
		DryRun: true, Conflict: fsops.ConflictOverwrite, DisableAudit: true, AllowUnguarded: true,
	}); err != nil {
		return operationPlanBuild{}, fmt.Errorf("validate complete operation request: %w", err)
	}
	if err := captureOperationGuards(cmd.Context(), &plan); err != nil {
		return operationPlanBuild{}, err
	}
	if _, err := fsops.Apply(cmd.Context(), plan, fsops.ApplyOptions{
		DryRun: true, Conflict: fsops.ConflictOverwrite, DisableAudit: true,
	}); err != nil {
		return operationPlanBuild{}, fmt.Errorf("validate guarded complete plan: %w", err)
	}
	summary := planSummary{
		Type: "plan_summary", Version: 1, InputOperations: inputCount,
		Operations: len(plan.Operations), DeduplicatedOperations: inputCount - len(plan.Operations),
		ActionCounts: make(map[string]int), ReclaimEstimateComplete: true,
	}
	for _, operation := range plan.Operations {
		summary.ActionCounts[string(operation.Action)]++
	}
	if flags.summary {
		if err := populateReclaimEstimate(cmd.Context(), plan.Operations, &summary); err != nil {
			return operationPlanBuild{}, err
		}
	}
	return operationPlanBuild{plan: plan, summary: summary}, nil
}

func nowUTC() time.Time {
	return time.Now().UTC()
}

func collectOperationRequests(cmd *cobra.Command, flags planFlags, args []string) ([]fsops.OperationRequest, error) {
	modes := 0
	if len(flags.sources) > 0 {
		modes++
	}
	if flags.files0From != "" {
		modes++
	}
	if flags.operationsFrom != "" {
		modes++
	}
	if modes > 1 {
		return nil, errors.New("--source, --files0-from, and --operations-from are mutually exclusive")
	}
	if flags.operationsFrom != "" {
		if len(args) != 0 {
			return nil, errors.New("--operations-from accepts no positional ACTION, SOURCE, or DESTINATION")
		}
		for _, name := range []string{"mode", "size", "uid", "gid", "archive-format", "recursive", "destination-dir"} {
			if cmd.Flags().Changed(name) {
				return nil, fmt.Errorf("--%s cannot be used with --operations-from; set it on each JSONL request", name)
			}
		}
		return readOperationRequests(cmd, flags.operationsFrom)
	}
	if modes == 0 {
		return legacyOperationRequest(cmd, flags, args)
	}
	if len(args) != 1 {
		return nil, errors.New("batch source input requires exactly one positional ACTION")
	}
	action, err := parseAction(args[0])
	if err != nil {
		return nil, err
	}
	template, err := uniformOperationRequest(cmd, flags, action)
	if err != nil {
		return nil, err
	}
	sources := append([]string(nil), flags.sources...)
	if flags.files0From != "" {
		sources, err = readNULSources(cmd, flags.files0From)
		if err != nil {
			return nil, err
		}
	}
	if len(sources) == 0 {
		return nil, errors.New("batch source input contains no paths")
	}
	if len(sources) > fsops.MaxPlanOperations {
		return nil, fmt.Errorf("batch source input exceeds maximum of %d operations", fsops.MaxPlanOperations)
	}
	requiresDestination := actionRequiresDestination(action)
	if requiresDestination {
		if action != fsops.ActionCopy && action != fsops.ActionMove && action != fsops.ActionRename {
			return nil, fmt.Errorf("batch %s requires explicit destinations; use --operations-from", action)
		}
		if strings.TrimSpace(flags.destinationDir) == "" {
			return nil, fmt.Errorf("batch %s requires --destination-dir", action)
		}
	} else if cmd.Flags().Changed("destination-dir") {
		return nil, fmt.Errorf("--destination-dir cannot be used with %s", action)
	}
	requests := make([]fsops.OperationRequest, 0, len(sources))
	for _, source := range sources {
		request := template
		request.Source = source
		if requiresDestination {
			request.Destination = filepath.Join(flags.destinationDir, filepath.Base(filepath.Clean(source)))
		}
		requests = append(requests, request)
	}
	return requests, nil
}

func legacyOperationRequest(cmd *cobra.Command, flags planFlags, args []string) ([]fsops.OperationRequest, error) {
	if len(args) == 0 {
		return nil, errors.New("plan requires ACTION and SOURCE")
	}
	action, err := parseAction(args[0])
	if err != nil {
		return nil, err
	}
	if actionRequiresDestination(action) && len(args) != 3 {
		return nil, fmt.Errorf("%s requires SOURCE and DESTINATION", action)
	}
	if !actionRequiresDestination(action) && len(args) != 2 {
		return nil, fmt.Errorf("%s accepts SOURCE only", action)
	}
	if cmd.Flags().Changed("destination-dir") {
		return nil, errors.New("--destination-dir is only valid with --source or --files0-from")
	}
	request, err := uniformOperationRequest(cmd, flags, action)
	if err != nil {
		return nil, err
	}
	request.Source = args[1]
	if len(args) == 3 {
		request.Destination = args[2]
	}
	return []fsops.OperationRequest{request}, nil
}

func uniformOperationRequest(cmd *cobra.Command, flags planFlags, action fsops.Action) (fsops.OperationRequest, error) {
	operation := fsops.Operation{Action: action, Recursive: flags.recursive, Format: flags.format}
	if cmd.Flags().Changed("mode") {
		mode, err := parseOperationMode(flags.mode)
		if err != nil {
			return fsops.OperationRequest{}, err
		}
		operation.Mode = &mode
	}
	if cmd.Flags().Changed("size") {
		size, err := parseSize(flags.size)
		if err != nil {
			return fsops.OperationRequest{}, fmt.Errorf("invalid --size %q: %w", flags.size, err)
		}
		operation.Size = &size
	}
	if cmd.Flags().Changed("uid") {
		if flags.uid < 0 {
			return fsops.OperationRequest{}, errors.New("--uid must be zero or greater")
		}
		operation.UID = &flags.uid
	}
	if cmd.Flags().Changed("gid") {
		if flags.gid < 0 {
			return fsops.OperationRequest{}, errors.New("--gid must be zero or greater")
		}
		operation.GID = &flags.gid
	}
	if err := validatePlanFlags(cmd, action, operation); err != nil {
		return fsops.OperationRequest{}, err
	}
	return fsops.OperationRequest{
		Action: action, Mode: operation.Mode, UID: operation.UID, GID: operation.GID,
		Size: operation.Size, Format: operation.Format, Recursive: operation.Recursive,
	}, nil
}

func actionRequiresDestination(action fsops.Action) bool {
	return action == fsops.ActionCopy || action == fsops.ActionMove || action == fsops.ActionRename ||
		action == fsops.ActionArchive || action == fsops.ActionExtract
}

func operationFromRequest(root string, request fsops.OperationRequest) (fsops.Operation, error) {
	action, err := parseAction(string(request.Action))
	if err != nil {
		return fsops.Operation{}, err
	}
	request.Action = action
	if err := validatePlanPathText("source", request.Source); err != nil {
		return fsops.Operation{}, err
	}
	if request.Destination != "" {
		if err := validatePlanPathText("destination", request.Destination); err != nil {
			return fsops.Operation{}, err
		}
	}
	if err := fsops.ValidateOperationRequest(request); err != nil {
		return fsops.Operation{}, err
	}
	source, err := fsops.ResolvePlanPath(root, request.Source)
	if err != nil {
		return fsops.Operation{}, fmt.Errorf("source: %w", err)
	}
	operation := fsops.Operation{
		Action: action, Source: source,
		Mode: request.Mode, UID: request.UID, GID: request.GID, Size: request.Size,
		Format: request.Format, Recursive: request.Recursive,
	}
	if request.Destination != "" {
		destination, destinationErr := fsops.ResolvePlanPath(root, request.Destination)
		if destinationErr != nil {
			return fsops.Operation{}, fmt.Errorf("destination: %w", destinationErr)
		}
		operation.Destination = destination
	}
	return operation, nil
}

func validatePlanPathText(name, path string) error {
	if strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("%s contains NUL", name)
	}
	if !utf8.ValidString(path) {
		return fmt.Errorf("%s is not valid UTF-8 and cannot be represented safely in plan JSONL", name)
	}
	return nil
}

func normalizePlanOperations(ctx context.Context, operations []fsops.Operation) ([]fsops.Operation, error) {
	seen := make(map[string]bool, len(operations))
	exact := make([]fsops.Operation, 0, len(operations))
	for _, operation := range operations {
		key, err := operationDeduplicationKey(operation)
		if err != nil {
			return nil, err
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		exact = append(exact, operation)
	}
	allDeletes := len(exact) > 0
	for _, operation := range exact {
		allDeletes = allDeletes && operation.Action == fsops.ActionDelete
	}
	if !allDeletes {
		return exact, nil
	}
	directories := make(map[string]bool)
	covered := make([]bool, len(exact))
	for candidateIndex, candidate := range exact {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for ancestorIndex, ancestor := range exact {
			if candidateIndex == ancestorIndex || !ancestor.Recursive {
				continue
			}
			isDirectory, known := directories[ancestor.Source]
			if !known {
				entry, err := fsinfo.Inspect(ancestor.Source, false)
				isDirectory = err == nil && entry.Kind == "directory"
				directories[ancestor.Source] = isDirectory
			}
			if !isDirectory {
				continue
			}
			if sameOperationPath(ancestor.Source, candidate.Source) {
				if !candidate.Recursive {
					covered[candidateIndex] = true
					break
				}
				continue
			}
			if operationPathContains(ancestor.Source, candidate.Source) {
				covered[candidateIndex] = true
				break
			}
		}
	}
	normalized := make([]fsops.Operation, 0, len(exact))
	for index, operation := range exact {
		if !covered[index] {
			normalized = append(normalized, operation)
		}
	}
	return normalized, nil
}

func operationDeduplicationKey(operation fsops.Operation) (string, error) {
	operation.ID, operation.Type = "", ""
	operation.Expected, operation.ExpectedDestination = nil, nil
	data, err := json.Marshal(operation)
	if err != nil {
		return "", fmt.Errorf("encode operation for duplicate detection: %w", err)
	}
	return string(data), nil
}

func sameOperationPath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func operationPathContains(parent, child string) bool {
	if sameOperationPath(parent, child) {
		return false
	}
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func captureOperationGuards(ctx context.Context, plan *fsops.Plan) error {
	for index := range plan.Operations {
		if err := ctx.Err(); err != nil {
			return err
		}
		operation := &plan.Operations[index]
		entry, err := fsinfo.Inspect(operation.Source, false)
		if err == nil {
			operation.Expected = &entry
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect operation %q source: %w", operation.ID, err)
		}
		if operation.Destination != "" {
			expectation, captureErr := fsinfo.CapturePath(operation.Destination)
			if captureErr != nil {
				return fmt.Errorf("inspect operation %q destination: %w", operation.ID, captureErr)
			}
			operation.ExpectedDestination = &expectation
		}
	}
	return nil
}

func readOperationRequests(cmd *cobra.Command, path string) ([]fsops.OperationRequest, error) {
	reader, closeInput, err := openPlanInput(cmd, path)
	if err != nil {
		return nil, err
	}
	requests, readErr := fsops.ReadOperationRequestsLimited(
		commandContextReader{ctx: cmd.Context(), reader: reader}, fsops.MaxPlanBytes,
	)
	if closeErr := closeInput(); readErr == nil {
		readErr = closeErr
	}
	if readErr != nil {
		return nil, fmt.Errorf("read operation request JSONL: %w", readErr)
	}
	if err := cmd.Context().Err(); err != nil {
		return nil, err
	}
	return requests, nil
}

func readNULSources(cmd *cobra.Command, path string) ([]string, error) {
	reader, closeInput, err := openPlanInput(cmd, path)
	if err != nil {
		return nil, err
	}
	data, readErr := readBoundedInput(commandContextReader{ctx: cmd.Context(), reader: reader}, fsops.MaxPlanBytes)
	if closeErr := closeInput(); readErr == nil {
		readErr = closeErr
	}
	if readErr != nil {
		return nil, fmt.Errorf("read NUL source input: %w", readErr)
	}
	if err := cmd.Context().Err(); err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("NUL source input is empty")
	}
	if data[len(data)-1] != 0 {
		return nil, errors.New("NUL source input is truncated: final record is not NUL-terminated")
	}
	records := bytes.Split(data[:len(data)-1], []byte{0})
	if len(records) > fsops.MaxPlanOperations {
		return nil, fmt.Errorf("NUL source input exceeds maximum of %d operations", fsops.MaxPlanOperations)
	}
	sources := make([]string, 0, len(records))
	for index, record := range records {
		if len(record) == 0 {
			return nil, fmt.Errorf("NUL source input record %d is empty", index+1)
		}
		if !utf8.Valid(record) {
			return nil, fmt.Errorf("NUL source input record %d is not valid UTF-8 and cannot be represented safely in plan JSONL", index+1)
		}
		sources = append(sources, string(record))
	}
	return sources, nil
}

func openPlanInput(cmd *cobra.Command, path string) (io.Reader, func() error, error) {
	if path == "-" {
		return cmd.InOrStdin(), func() error { return nil }, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open plan input %q: %w", path, err)
	}
	return file, file.Close, nil
}

func readBoundedInput(reader io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("input exceeds maximum size of %d bytes", maxBytes)
	}
	return data, nil
}

type reclaimIdentity struct {
	device uint64
	file   uint64
}

type reclaimHardlink struct {
	allocated int64
	links     uint64
	planned   uint64
}

func populateReclaimEstimate(ctx context.Context, operations []fsops.Operation, summary *planSummary) error {
	hardlinks := make(map[reclaimIdentity]reclaimHardlink)
	for _, operation := range operations {
		if operation.Action != fsops.ActionDelete {
			continue
		}
		if err := estimateDeleteOperation(ctx, operation, summary, hardlinks); err != nil {
			return err
		}
	}
	for _, item := range hardlinks {
		if item.links > 0 && item.planned < item.links {
			continue
		}
		summary.DeleteReclaimBytes = saturatedAdd(summary.DeleteReclaimBytes, item.allocated)
	}
	return nil
}

func estimateDeleteOperation(
	ctx context.Context,
	operation fsops.Operation,
	summary *planSummary,
	hardlinks map[reclaimIdentity]reclaimHardlink,
) error {
	addPath := func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry, err := fsinfo.Inspect(path, false)
		if err != nil {
			summary.ReclaimEstimateComplete = false
			summary.ReclaimEstimateErrors++
			return nil
		}
		if entry.Kind == inspectKindFile && entry.Identity.Valid {
			key := reclaimIdentity{device: entry.Identity.Device, file: entry.Identity.File}
			item := hardlinks[key]
			item.allocated, item.links, item.planned = max(item.allocated, entry.Allocated), max(item.links, entry.Links), item.planned+1
			hardlinks[key] = item
			return nil
		}
		if entry.Allocated > 0 {
			summary.DeleteReclaimBytes = saturatedAdd(summary.DeleteReclaimBytes, entry.Allocated)
		}
		return nil
	}
	if !operation.Recursive {
		return addPath(operation.Source)
	}
	return filepath.WalkDir(operation.Source, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			summary.ReclaimEstimateComplete = false
			summary.ReclaimEstimateErrors++
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		return addPath(path)
	})
}

func saturatedAdd(left, right int64) int64 {
	if right <= 0 {
		return left
	}
	if left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func writePlanSummary(cmd *cobra.Command, summary planSummary) error {
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	encoder := json.NewEncoder(cmd.ErrOrStderr())
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(summary); err != nil {
		return fmt.Errorf("write plan summary: %w", err)
	}
	return nil
}
