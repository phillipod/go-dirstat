package fsops

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

const (
	archiveFormatZIP   = "zip"
	archiveFormatTar   = "tar"
	archiveFormatTarGZ = "tar.gz"
	windowsOS          = "windows"
)

type mutationFilesystem struct {
	rename      func(string, string) error
	publish     func(string, string) error
	remove      func(string) error
	removeAll   func(string) error
	mkdirTemp   func(string, string) (string, error)
	copy        func(io.Writer, io.Reader) (int64, error)
	sync        func(*os.File) error
	close       func(*os.File) error
	crossDevice func(error) bool
}

func defaultMutationFilesystem() mutationFilesystem {
	return mutationFilesystem{
		rename:      os.Rename,
		publish:     renameNoReplace,
		remove:      os.Remove,
		removeAll:   os.RemoveAll,
		mkdirTemp:   os.MkdirTemp,
		copy:        io.Copy,
		sync:        (*os.File).Sync,
		close:       (*os.File).Close,
		crossDevice: isCrossDevice,
	}
}

func (o ApplyOptions) mutationFilesystem() mutationFilesystem {
	if o.filesystem != nil {
		return *o.filesystem
	}
	return defaultMutationFilesystem()
}

type partialMutationError struct {
	cause error
}

func (e *partialMutationError) Error() string { return e.cause.Error() }

func (e *partialMutationError) Unwrap() error { return e.cause }

func partialMutation(cause error) error {
	var partial *partialMutationError
	if errors.As(cause, &partial) {
		return cause
	}
	return &partialMutationError{cause: cause}
}

func isPartialMutation(err error) bool {
	var partial *partialMutationError
	return errors.As(err, &partial)
}

type publishedMutationError struct {
	cause error
}

func (e *publishedMutationError) Error() string { return e.cause.Error() }

func (e *publishedMutationError) Unwrap() error { return e.cause }

func publishedMutation(cause error) error {
	return &publishedMutationError{cause: partialMutation(cause)}
}

func isPublishedMutation(err error) bool {
	var published *publishedMutationError
	return errors.As(err, &published)
}

type ownedDestinationError struct {
	cause error
}

func (e *ownedDestinationError) Error() string { return e.cause.Error() }

func (e *ownedDestinationError) Unwrap() error { return e.cause }

func ownedDestinationMutation(cause error) error {
	return &ownedDestinationError{cause: partialMutation(cause)}
}

func isOwnedDestinationMutation(err error) bool {
	var owned *ownedDestinationError
	return errors.As(err, &owned)
}

type auditLog interface {
	io.Writer
	Sync() error
	Close() error
}

type auditSession struct {
	writer io.Writer
	sync   func() error
	close  func() error
}

func DefaultAuditPath(root string) string { return filepath.Join(root, ".dirstat-audit.jsonl") }

func OpenAudit(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	f, err := openAuditFile(path)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func applyPlan(ctx context.Context, plan Plan, opts ApplyOptions) (results []Result, returnErr error) {
	if !supportedVersion(plan.Header.Version) {
		return nil, fmt.Errorf("unsupported plan version %d", plan.Header.Version)
	}
	if strings.TrimSpace(plan.Header.Root) == "" {
		return nil, errors.New("plan root is required")
	}
	root, err := filepath.Abs(filepath.Clean(plan.Header.Root))
	if err != nil {
		return nil, fmt.Errorf("plan root: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return nil, fmt.Errorf("plan root %q is not a directory", root)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve plan root: %w", err)
	}
	policy := opts.Conflict
	if policy == "" {
		policy = ConflictFail
	}
	if policy != ConflictFail && policy != ConflictOverwrite {
		return nil, fmt.Errorf("unknown conflict policy %q", policy)
	}
	if policy == ConflictOverwrite && plan.Header.Version < PlanVersion {
		return nil, fmt.Errorf("plan version %d cannot authorize overwrite; regenerate a version %d plan with a destination guard", plan.Header.Version, PlanVersion)
	}
	filesystem := opts.mutationFilesystem()
	if err := prevalidatePlan(ctx, root, plan, policy, opts.AllowUnguarded, filesystem); err != nil {
		wrapped := fmt.Errorf("prevalidate plan: %w", err)
		var validationErr *planPrevalidationError
		if !errors.As(err, &validationErr) {
			return nil, wrapped
		}
		now := time.Now().UTC()
		return []Result{{
			Type: resultRecordType, Version: PlanVersion,
			OperationID: validationErr.operation.ID, Action: validationErr.operation.Action,
			Status: ResultStatusError, DryRun: opts.DryRun,
			AuditIntentStatus: AuditStatusDisabled, AuditStatus: AuditStatusDisabled,
			Error:     wrapped.Error(),
			StartedAt: now, FinishedAt: now,
		}}, wrapped
	}
	audit, err := prepareAudit(root, plan, opts)
	if err != nil {
		return nil, err
	}
	if audit != nil && audit.close != nil {
		defer func() {
			if closeErr := audit.close(); closeErr != nil {
				wrapped := fmt.Errorf("close audit log: %w", closeErr)
				if len(results) > 0 {
					markAuditFailure(&results[len(results)-1], wrapped)
				}
				returnErr = errors.Join(returnErr, wrapped)
			}
		}()
	}

	results = make([]Result, 0, len(plan.Operations))
	created := make(map[string]bool)
	for _, op := range plan.Operations {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		started := time.Now().UTC()
		r := Result{
			Type: resultRecordType, Version: PlanVersion,
			OperationID: op.ID, Action: op.Action, Status: ResultStatusOK,
			DryRun: opts.DryRun, StartedAt: started,
		}
		if audit == nil {
			r.AuditIntentStatus, r.AuditStatus = AuditStatusDisabled, AuditStatusDisabled
		} else {
			r.AuditIntentStatus, r.AuditStatus = AuditStatusNotAttempted, AuditStatusNotAttempted
		}
		if intentErr := persistAuditIntent(audit, &r); intentErr != nil {
			r.Status = ResultStatusError
			r.Error = intentErr.Error()
			r.AuditIntentStatus = AuditStatusFailed
			r.FinishedAt = time.Now().UTC()
			results = append(results, r)
			return results, intentErr
		}
		sourceKey := op.Source
		if !filepath.IsAbs(sourceKey) {
			sourceKey = filepath.Join(root, sourceKey)
		}
		sourceKey, _ = filepath.Abs(filepath.Clean(sourceKey))
		var err error
		if !opts.DryRun {
			err = execute(ctx, root, op, false, policy, opts.AllowUnguarded || created[sourceKey], filesystem)
		}
		if err != nil {
			r.Status, r.Error = ResultStatusError, err.Error()
			if isPartialMutation(err) {
				r.Status, r.MayHaveMutated = ResultStatusPartial, true
			}
		} else if !opts.DryRun {
			r.MutationCompleted = true
		}
		r.FinishedAt = time.Now().UTC()
		results = append(results, r)
		if err == nil {
			createdPath := op.Destination
			if op.Action == ActionMkdir || op.Action == ActionTouch {
				createdPath = op.Source
			}
			if createdPath != "" {
				if !filepath.IsAbs(createdPath) {
					createdPath = filepath.Join(root, createdPath)
				}
				if abs, absErr := filepath.Abs(filepath.Clean(createdPath)); absErr == nil {
					created[abs] = true
				}
			}
		}
		if auditErr := persistAuditOutcome(audit, &results[len(results)-1]); auditErr != nil {
			markAuditFailure(&results[len(results)-1], auditErr)
			if err != nil {
				return results, errors.Join(err, auditErr)
			}
			return results, auditErr
		}
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

func prepareAudit(root string, plan Plan, opts ApplyOptions) (*auditSession, error) {
	if opts.Audit != nil {
		session := &auditSession{writer: opts.Audit}
		if syncer, ok := opts.Audit.(interface{ Sync() error }); ok {
			session.sync = syncer.Sync
		}
		return session, nil
	}
	if opts.DisableAudit {
		return nil, nil
	}
	path := opts.AuditPath
	if path == "" {
		path = DefaultAuditPath(root)
	}
	if err := validateAuditPlacement(root, plan, path); err != nil {
		return nil, fmt.Errorf("audit log placement: %w", err)
	}
	factory := opts.auditFactory
	if factory == nil {
		factory = func(auditPath string) (auditLog, error) { return OpenAudit(auditPath) }
	}
	log, err := factory(path)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	return &auditSession{writer: log, sync: log.Sync, close: log.Close}, nil
}

func persistAuditOutcome(audit *auditSession, result *Result) error {
	if audit == nil {
		result.AuditStatus = AuditStatusDisabled
		return nil
	}
	record := *result
	record.AuditPhase = AuditPhaseOutcome
	record.AuditStatus = AuditStatusWritten
	if err := WriteResult(audit.writer, record); err != nil {
		return fmt.Errorf("write audit outcome: %w", err)
	}
	if audit.sync == nil {
		result.AuditStatus = AuditStatusWritten
		return nil
	}
	if err := audit.sync(); err != nil {
		return fmt.Errorf("sync audit outcome: %w", err)
	}
	result.AuditStatus = AuditStatusDurable
	return nil
}

func persistAuditIntent(audit *auditSession, result *Result) error {
	if audit == nil {
		result.AuditIntentStatus = AuditStatusDisabled
		return nil
	}
	record := *result
	record.Status = ResultStatusIntent
	record.AuditPhase = AuditPhaseIntent
	record.AuditIntentStatus = AuditStatusWritten
	record.AuditStatus = AuditStatusNotAttempted
	record.FinishedAt = record.StartedAt
	if err := WriteResult(audit.writer, record); err != nil {
		return fmt.Errorf("write audit intent: %w", err)
	}
	if audit.sync == nil {
		result.AuditIntentStatus = AuditStatusWritten
		return nil
	}
	if err := audit.sync(); err != nil {
		return fmt.Errorf("sync audit intent: %w", err)
	}
	result.AuditIntentStatus = AuditStatusDurable
	return nil
}

func markAuditFailure(result *Result, auditErr error) {
	result.AuditStatus = AuditStatusFailed
	detail := auditErr.Error()
	if result.MutationCompleted {
		detail = "filesystem mutation completed but " + detail
	}
	if result.Error == "" {
		result.Error = detail
	} else {
		result.Error = errors.Join(errors.New(result.Error), errors.New(detail)).Error()
	}
	if result.MutationCompleted || result.MayHaveMutated {
		result.Status = ResultStatusPartial
		result.MayHaveMutated = true
	} else {
		result.Status = ResultStatusError
	}
}

func validateAuditPlacement(root string, plan Plan, auditPath string) error {
	resolvedAudit, err := canonicalNoFollowPath(auditPath)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", auditPath, err)
	}
	for _, operation := range plan.Operations {
		source, sourceErr := validationPath(root, operation.Source)
		if sourceErr != nil {
			return sourceErr
		}
		if samePath(source, resolvedAudit) || pathContains(source, resolvedAudit) {
			return fmt.Errorf("operation %q source contains the audit log", operation.ID)
		}
		if operation.Destination == "" {
			continue
		}
		destination, destinationErr := validationPath(root, operation.Destination)
		if destinationErr != nil {
			return destinationErr
		}
		if samePath(destination, resolvedAudit) || pathContains(destination, resolvedAudit) {
			return fmt.Errorf("operation %q destination contains the audit log", operation.ID)
		}
	}
	return nil
}

func execute(
	ctx context.Context,
	root string,
	op Operation,
	dry bool,
	policy ConflictPolicy,
	allowUnguarded bool,
	filesystem mutationFilesystem,
) error {
	if op.ID == "" {
		return errors.New("operation id is required")
	}
	if err := validateAction(op.Action); err != nil {
		return err
	}
	src, err := confinedPath(root, op.Source, actionAllowsMissingSource(op.Action))
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if src == root && (op.Action == ActionDelete || op.Action == ActionMove || op.Action == ActionRename) {
		return fmt.Errorf("refusing to %s the plan root", op.Action)
	}
	if op.Expected == nil && !allowUnguarded {
		if _, statErr := os.Lstat(src); statErr == nil {
			return errors.New("existing source requires expected metadata guard")
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return statErr
		}
	}
	var dst string
	if op.Destination != "" {
		dst, err = confinedPath(root, op.Destination, true)
		if err != nil {
			return fmt.Errorf("destination: %w", err)
		}
	}
	if dst != "" && policy == ConflictOverwrite && op.ExpectedDestination == nil && !allowUnguarded {
		return errors.New("overwrite destination requires expected destination guard")
	}
	if op.ExpectedDestination != nil {
		if dst == "" {
			return errors.New("expected destination guard requires a destination")
		}
		if err := checkDestinationExpectation(*op.ExpectedDestination, dst); err != nil {
			return err
		}
	}
	if op.Expected != nil {
		if op.Expected.Path != "" {
			expectedPath, pathErr := canonicalNoFollowPath(op.Expected.Path)
			if pathErr != nil || !samePath(expectedPath, src) {
				return errors.New("stale source: expected path does not match operation source")
			}
		}
		actual, inspectErr := fsinfo.Inspect(src, false)
		if inspectErr != nil {
			return fmt.Errorf("stale source: %w", inspectErr)
		}
		if err := checkExpected(*op.Expected, actual); err != nil {
			return err
		}
	}
	if err := validateParameters(op, src, dst); err != nil {
		return err
	}
	if err := preflight(ctx, op, src, dst, policy); err != nil {
		return err
	}
	var validatedArchive *archiveLayout
	if op.Action == ActionExtract {
		validatedArchive, err = inspectArchive(ctx, src, op.Format)
		if err != nil {
			return err
		}
	}
	if dry {
		return nil
	}
	// Preflight can be intentionally expensive (for example, validating an
	// archive). Re-resolve confinement and stale metadata immediately before
	// the mutating syscall so a change during preflight is not trusted.
	if checked, err := confinedPath(root, src, actionAllowsMissingSource(op.Action)); err != nil || checked != src {
		if err != nil {
			return fmt.Errorf("source changed after preflight: %w", err)
		}
		return errors.New("source changed after preflight")
	}
	if dst != "" {
		if checked, err := confinedPath(root, dst, true); err != nil || checked != dst {
			if err != nil {
				return fmt.Errorf("destination changed after preflight: %w", err)
			}
			return errors.New("destination changed after preflight")
		}
	}
	if op.Expected != nil {
		actual, err := fsinfo.Inspect(src, false)
		if err != nil {
			return fmt.Errorf("stale source after preflight: %w", err)
		}
		if err := checkExpected(*op.Expected, actual); err != nil {
			return err
		}
	}
	if op.ExpectedDestination != nil {
		if err := checkDestinationExpectation(*op.ExpectedDestination, dst); err != nil {
			return err
		}
	}

	switch op.Action {
	case ActionDelete:
		if op.Recursive {
			return removeTreeCancellable(ctx, src, filesystem)
		}
		return filesystem.remove(src)
	case ActionCopy:
		return copyPathGuarded(ctx, src, dst, policy, op.ExpectedDestination, filesystem)
	case ActionMove, ActionRename:
		return movePathGuarded(ctx, src, dst, policy, op.ExpectedDestination, filesystem)
	case ActionMkdir:
		mode := os.FileMode(0o755)
		if op.Mode != nil {
			mode = operationMode(*op.Mode)
		}
		return os.Mkdir(src, mode)
	case ActionTouch:
		if err := rejectSymlink(src); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return touch(src, op.Mode)
	case ActionTruncate:
		if err := rejectSymlink(src); err != nil {
			return err
		}
		return os.Truncate(src, *op.Size)
	case ActionChmod:
		if op.Mode == nil {
			return errors.New("chmod mode is required")
		}
		return chmodNoFollow(src, operationMode(*op.Mode))
	case ActionChown:
		uid, gid := -1, -1
		if op.UID != nil {
			uid = *op.UID
		}
		if op.GID != nil {
			gid = *op.GID
		}
		if uid == -1 && gid == -1 {
			return errors.New("chown uid or gid is required")
		}
		return chownNoFollow(src, uid, gid)
	case ActionArchive:
		return archivePathGuarded(ctx, src, dst, op.Format, policy, op.ExpectedDestination, filesystem)
	case ActionExtract:
		if err := rejectSymlink(src); err != nil {
			return err
		}
		return extractArchiveGuarded(ctx, src, dst, op.Format, policy, validatedArchive, op.ExpectedDestination, filesystem)
	default:
		return fmt.Errorf("unsupported action %q", op.Action)
	}
}

func preflight(ctx context.Context, op Operation, src, dst string, policy ConflictPolicy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dst != "" {
		parent, err := os.Stat(filepath.Dir(dst))
		if err != nil {
			return fmt.Errorf("destination parent: %w", err)
		}
		if !parent.IsDir() {
			return errors.New("destination parent is not a directory")
		}
		if err := destinationReady(dst, policy); err != nil {
			return err
		}
	}
	switch op.Action {
	case ActionMkdir, ActionTouch:
		parent, err := os.Stat(filepath.Dir(src))
		if err != nil {
			return fmt.Errorf("source parent: %w", err)
		}
		if !parent.IsDir() {
			return errors.New("source parent is not a directory")
		}
	case ActionDelete, ActionCopy, ActionMove, ActionRename, ActionTruncate, ActionChmod, ActionChown, ActionArchive, ActionExtract:
	}
	switch op.Action {
	case ActionTouch, ActionTruncate, ActionChmod, ActionChown, ActionExtract:
		if _, err := os.Lstat(src); err == nil {
			if err := rejectSymlink(src); err != nil {
				return err
			}
		}
	case ActionDelete, ActionCopy, ActionMove, ActionRename, ActionMkdir, ActionArchive:
	}
	if op.Action == ActionCopy {
		info, err := os.Lstat(src)
		if err != nil {
			return err
		}
		if info.IsDir() && within(src, dst) {
			return fmt.Errorf("destination %q is inside source %q", dst, src)
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("cannot copy %s", info.Mode())
		}
	}
	if op.Action == ActionArchive {
		if info, err := os.Lstat(src); err != nil {
			return err
		} else if info.IsDir() && within(src, dst) {
			return fmt.Errorf("archive destination %q is inside source %q", dst, src)
		}
	}
	if op.Action == ActionExtract {
		info, err := os.Lstat(src)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("extract source must be a regular file")
		}
	}
	return nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to follow final symlink %q", path)
	}
	return nil
}

func validateAction(a Action) error {
	switch a {
	case ActionDelete, ActionCopy, ActionMove, ActionRename, ActionMkdir, ActionTouch, ActionTruncate, ActionChmod, ActionChown, ActionArchive, ActionExtract:
		return nil
	default:
		return fmt.Errorf("unsupported action %q", a)
	}
}

func actionAllowsMissingSource(a Action) bool { return a == ActionMkdir || a == ActionTouch }

func validateParameters(op Operation, src, dst string) error {
	if err := validateOperationForOS(runtime.GOOS, op); err != nil {
		return err
	}
	switch op.Action {
	case ActionCopy, ActionMove, ActionRename, ActionArchive, ActionExtract:
		if dst == "" {
			return errors.New("destination is required")
		}
	case ActionTruncate:
		if op.Size == nil || *op.Size < 0 {
			return errors.New("valid truncate size is required")
		}
	case ActionChmod:
		if op.Mode == nil {
			return errors.New("chmod mode is required")
		}
	case ActionChown:
		if op.UID == nil && op.GID == nil {
			return errors.New("chown uid or gid is required")
		}
	case ActionDelete, ActionMkdir, ActionTouch:
	}
	if (op.Action == ActionArchive || op.Action == ActionExtract) && archiveFormat(src, op.Format) == "invalid" {
		return fmt.Errorf("unsupported archive format %q", op.Format)
	}
	if src == "" {
		return errors.New("source is required")
	}
	return nil
}

func validateOperationForOS(goos string, operation Operation) error {
	if goos != windowsOS {
		return nil
	}
	if operation.Action == ActionChmod || operation.Action == ActionChown {
		return fmt.Errorf("%s is unsupported on windows", operation.Action)
	}
	if operation.Mode != nil {
		return errors.New("explicit POSIX modes are unsupported on windows")
	}
	return nil
}

func checkExpected(expected, actual fsinfo.Entry) error {
	if expected.Identity.Valid {
		if !actual.Identity.Valid || expected.Identity.Device != actual.Identity.Device || expected.Identity.File != actual.Identity.File {
			return errors.New("stale source: filesystem identity changed")
		}
	}
	if expected.Size != actual.Size {
		return fmt.Errorf("stale source: size changed from %d to %d", expected.Size, actual.Size)
	}
	if !expected.ModTime.Equal(actual.ModTime) {
		return errors.New("stale source: modification time changed")
	}
	return nil
}

func checkDestinationExpectation(expected fsinfo.PathExpectation, destination string) error {
	if expected.Path == "" {
		return errors.New("invalid destination expectation: path is required")
	}
	expectedPath, err := canonicalNoFollowPath(expected.Path)
	if err != nil || !samePath(expectedPath, destination) {
		return errors.New("stale destination: expected path does not match operation destination")
	}
	actual, err := fsinfo.CapturePath(destination)
	if err != nil {
		return fmt.Errorf("stale destination: %w", err)
	}
	if expected.Exists != actual.Exists {
		if expected.Exists {
			return errors.New("stale destination: reviewed destination no longer exists")
		}
		return errors.New("stale destination: destination was created after review")
	}
	if !expected.Exists {
		if expected.Entry != nil {
			return errors.New("invalid destination expectation: absent path has entry metadata")
		}
		return nil
	}
	if expected.Entry == nil || actual.Entry == nil {
		return errors.New("invalid destination expectation: existing path requires entry metadata")
	}
	if !fsinfo.SameObject(*expected.Entry, *actual.Entry) {
		return errors.New("stale destination: filesystem object changed after review")
	}
	return nil
}

func confinedPath(root, path string, allowMissing bool) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(abs)
	resolved, err := evalExisting(parent)
	if err != nil {
		return "", err
	}
	abs = filepath.Join(resolved, filepath.Base(abs))
	if !within(root, abs) {
		return "", fmt.Errorf("resolved path %q escapes root %q", abs, root)
	}
	if _, err := os.Lstat(abs); err != nil && (!allowMissing || !errors.Is(err, fs.ErrNotExist)) {
		return "", err
	}
	return abs, nil
}

func canonicalNoFollowPath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	parent, err := evalExisting(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func evalExisting(path string) (string, error) {
	cur := path
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(append([]string{resolved}, reverse(tail)...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

func reverse(v []string) []string {
	for i, j := 0, len(v)-1; i < j; i, j = i+1, j-1 {
		v[i], v[j] = v[j], v[i]
	}
	return v
}

func within(root, path string) bool {
	if runtime.GOOS == windowsOS {
		root, path = strings.ToLower(root), strings.ToLower(path)
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func samePath(a, b string) bool {
	if runtime.GOOS == windowsOS {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func destinationReady(dst string, policy ConflictPolicy) error {
	_, err := os.Lstat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if policy == ConflictFail {
		return fmt.Errorf("destination %q already exists", dst)
	}
	return nil
}

// withDestination makes overwrite replacement rollback-safe. The existing
// destination is renamed within its parent (an atomic same-filesystem step),
// restored if construction fails, and removed only after the replacement is
// complete.
func withDestinationGuarded(
	dst string,
	policy ConflictPolicy,
	expected *fsinfo.PathExpectation,
	filesystem mutationFilesystem,
	build func() error,
) error {
	if expected != nil {
		if err := checkDestinationExpectation(*expected, dst); err != nil {
			return err
		}
	}
	_, err := os.Lstat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return build()
	}
	if err != nil {
		return err
	}
	if policy == ConflictFail {
		return fmt.Errorf("destination %q already exists", dst)
	}
	placeholder, err := os.CreateTemp(filepath.Dir(dst), ".dirstat-backup-*")
	if err != nil {
		return err
	}
	backup := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		_ = filesystem.remove(backup)
		return err
	}
	if err := filesystem.remove(backup); err != nil {
		return err
	}
	if expected != nil {
		if err := checkDestinationExpectation(*expected, dst); err != nil {
			return err
		}
	}
	if err := filesystem.rename(dst, backup); err != nil {
		return err
	}
	if expected != nil && expected.Exists {
		actual, inspectErr := fsinfo.Inspect(backup, false)
		if inspectErr != nil || expected.Entry == nil || !fsinfo.SameObject(*expected.Entry, actual) {
			restoreErr := filesystem.publish(backup, dst)
			if restoreErr != nil {
				return partialMutation(fmt.Errorf("stale destination changed during overwrite (reviewed destination retained at %q; restore failed: %v)", backup, restoreErr))
			}
			return errors.New("stale destination: filesystem object changed during overwrite")
		}
	}
	if err := build(); err != nil {
		if isPublishedMutation(err) {
			if cleanupErr := filesystem.removeAll(backup); cleanupErr != nil {
				return partialMutation(errors.Join(err, fmt.Errorf("remove replaced destination backup: %w", cleanupErr)))
			}
			return err
		}
		if isOwnedDestinationMutation(err) {
			if cleanupErr := filesystem.removeAll(dst); cleanupErr != nil {
				return partialMutation(errors.Join(err, fmt.Errorf("remove incomplete replacement: %w", cleanupErr)))
			}
			if restoreErr := filesystem.publish(backup, dst); restoreErr != nil {
				return partialMutation(errors.Join(err, fmt.Errorf("restore reviewed destination retained at %q: %w", backup, restoreErr)))
			}
			return err
		}
		if _, destinationErr := os.Lstat(dst); destinationErr == nil {
			return partialMutation(errors.Join(err, fmt.Errorf(
				"destination appeared while replacement was being built; reviewed destination retained at %q",
				backup,
			)))
		} else if !errors.Is(destinationErr, fs.ErrNotExist) {
			return partialMutation(errors.Join(err, fmt.Errorf("inspect replacement destination (reviewed destination retained at %q): %w", backup, destinationErr)))
		}
		if restoreErr := filesystem.publish(backup, dst); restoreErr != nil {
			return partialMutation(errors.Join(err, fmt.Errorf("restore reviewed destination retained at %q: %w", backup, restoreErr)))
		}
		return err
	}
	if err := filesystem.removeAll(backup); err != nil {
		return partialMutation(fmt.Errorf("replacement published but remove destination backup: %w", err))
	}
	return nil
}

func touch(path string, mode *uint32) error {
	now := time.Now()
	if _, err := os.Lstat(path); err == nil {
		return os.Chtimes(path, now, now)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	perm := os.FileMode(0o644)
	if mode != nil {
		perm = operationMode(*mode)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	return f.Close()
}

func operationMode(mode uint32) os.FileMode {
	result := os.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		result |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		result |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		result |= os.ModeSticky
	}
	return result
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func movePathGuarded(
	ctx context.Context,
	src, dst string,
	policy ConflictPolicy,
	expected *fsinfo.PathExpectation,
	filesystem mutationFilesystem,
) error {
	return withDestinationGuarded(dst, policy, expected, filesystem, func() error {
		return movePathNew(ctx, src, dst, filesystem)
	})
}

func movePathNew(ctx context.Context, src, dst string, filesystem mutationFilesystem) error {
	if err := filesystem.publish(src, dst); err == nil {
		return nil
	} else if !filesystem.crossDevice(err) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot, err := captureSourceSnapshot(ctx, src)
	if err != nil {
		return fmt.Errorf("capture source before cross-device move: %w", err)
	}
	parent := filepath.Dir(dst)
	tmp, err := filesystem.mkdirTemp(parent, ".dirstat-move-")
	if err != nil {
		return err
	}
	defer func() { _ = filesystem.removeAll(tmp) }()
	if err := filesystem.remove(tmp); err != nil {
		return err
	}
	if err := copyPathNew(ctx, src, tmp, filesystem); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifySourceSnapshot(ctx, src, snapshot); err != nil {
		return fmt.Errorf("source changed while staging cross-device move: %w", err)
	}
	if err := filesystem.publish(tmp, dst); err != nil {
		return err
	}
	if _, err := cleanupSourceSnapshot(ctx, src, snapshot, filesystem); err != nil {
		return publishedMutation(fmt.Errorf("destination published but source cleanup incomplete: %w", err))
	}
	return nil
}

type sourceSnapshotEntry struct {
	relative string
	entry    fsinfo.Entry
}

type sourceSnapshot struct {
	entries []sourceSnapshotEntry
}

func captureSourceSnapshot(ctx context.Context, source string) (sourceSnapshot, error) {
	snapshot := sourceSnapshot{}
	err := filepath.WalkDir(source, func(path string, _ fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		entry, err := fsinfo.Inspect(path, false)
		if err != nil {
			return err
		}
		snapshot.entries = append(snapshot.entries, sourceSnapshotEntry{relative: relative, entry: entry})
		return nil
	})
	if err != nil {
		return sourceSnapshot{}, err
	}
	return snapshot, nil
}

func verifySourceSnapshot(ctx context.Context, source string, expected sourceSnapshot) error {
	actual, err := captureSourceSnapshot(ctx, source)
	if err != nil {
		return err
	}
	if len(expected.entries) != len(actual.entries) {
		return fmt.Errorf("entry set changed from %d to %d objects", len(expected.entries), len(actual.entries))
	}
	for i := range expected.entries {
		want, got := expected.entries[i], actual.entries[i]
		if want.relative != got.relative {
			return fmt.Errorf("entry set changed near %q", want.relative)
		}
		if !sameSnapshotObject(want.entry, got.entry) {
			return fmt.Errorf("entry %q changed", want.relative)
		}
	}
	return nil
}

func sameSnapshotObject(expected, actual fsinfo.Entry) bool {
	return fsinfo.SameObject(expected, actual) &&
		expected.Mode == actual.Mode &&
		expected.UID == actual.UID &&
		expected.GID == actual.GID &&
		expected.Symlink == actual.Symlink
}

func sameCleanupObject(expected, actual fsinfo.Entry) bool {
	if expected.Identity.Valid {
		if !actual.Identity.Valid || expected.Identity.Device != actual.Identity.Device || expected.Identity.File != actual.Identity.File {
			return false
		}
	}
	if expected.Kind != actual.Kind || expected.Mode != actual.Mode ||
		expected.UID != actual.UID || expected.GID != actual.GID {
		return false
	}
	if expected.Kind == objectKindDirectory {
		return true
	}
	return expected.Size == actual.Size &&
		expected.ModTime.Equal(actual.ModTime) &&
		expected.Symlink == actual.Symlink
}

func cleanupSourceSnapshot(
	ctx context.Context,
	source string,
	snapshot sourceSnapshot,
	filesystem mutationFilesystem,
) (int, error) {
	removed := 0
	byRelative := make(map[string]fsinfo.Entry, len(snapshot.entries))
	for _, item := range snapshot.entries {
		byRelative[item.relative] = item.entry
	}
	for i := len(snapshot.entries) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		item := snapshot.entries[i]
		if err := validateSourceAncestors(source, item.relative, byRelative); err != nil {
			return removed, err
		}
		path := filepath.Join(source, item.relative)
		actual, err := fsinfo.Inspect(path, false)
		if err != nil {
			return removed, fmt.Errorf("inspect captured source %q: %w", item.relative, err)
		}
		if !sameCleanupObject(item.entry, actual) {
			return removed, fmt.Errorf("captured source %q changed after staging", item.relative)
		}
		if err := filesystem.remove(path); err != nil {
			return removed, fmt.Errorf("remove captured source %q: %w", item.relative, err)
		}
		removed++
	}
	return removed, nil
}

func removeTreeCancellable(ctx context.Context, source string, filesystem mutationFilesystem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot, err := captureSourceSnapshot(ctx, source)
	if err != nil {
		return fmt.Errorf("capture recursive delete set: %w", err)
	}
	removed, err := cleanupSourceSnapshot(ctx, source, snapshot, filesystem)
	if err == nil {
		return nil
	}
	wrapped := fmt.Errorf("recursive delete stopped after removing %d of %d objects: %w", removed, len(snapshot.entries), err)
	if removed > 0 {
		return partialMutation(wrapped)
	}
	return wrapped
}

func validateSourceAncestors(source, relative string, expected map[string]fsinfo.Entry) error {
	ancestor := filepath.Dir(relative)
	for {
		want, ok := expected[ancestor]
		if !ok {
			return fmt.Errorf("captured source ancestor %q is missing from snapshot", ancestor)
		}
		actual, err := fsinfo.Inspect(filepath.Join(source, ancestor), false)
		if err != nil {
			return fmt.Errorf("inspect captured source ancestor %q: %w", ancestor, err)
		}
		if !sameCleanupObject(want, actual) {
			return fmt.Errorf("captured source ancestor %q changed after staging", ancestor)
		}
		if ancestor == "." {
			return nil
		}
		ancestor = filepath.Dir(ancestor)
	}
}

func copyPathGuarded(
	ctx context.Context,
	src, dst string,
	policy ConflictPolicy,
	expected *fsinfo.PathExpectation,
	filesystem mutationFilesystem,
) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() && within(src, dst) {
		return fmt.Errorf("destination %q is inside source %q", dst, src)
	}
	return withDestinationGuarded(dst, policy, expected, filesystem, func() error {
		if info.IsDir() {
			return copyDirectoryStaged(ctx, src, dst, filesystem)
		}
		return copyPathNew(ctx, src, dst, filesystem)
	})
}

func copyDirectoryStaged(
	ctx context.Context,
	src, dst string,
	filesystem mutationFilesystem,
) (returnErr error) {
	snapshot, err := captureSourceSnapshot(ctx, src)
	if err != nil {
		return fmt.Errorf("capture source before directory copy: %w", err)
	}
	stage, err := filesystem.mkdirTemp(filepath.Dir(dst), ".dirstat-copy-")
	if err != nil {
		return fmt.Errorf("create directory copy staging area: %w", err)
	}
	published := false
	defer func() {
		if published {
			return
		}
		makeTreeRemovable(stage)
		if cleanupErr := filesystem.removeAll(stage); cleanupErr != nil {
			wrapped := fmt.Errorf("remove directory copy staging area %q: %w", stage, cleanupErr)
			returnErr = partialMutation(errors.Join(returnErr, wrapped))
		}
	}()
	if err := copyDirectoryIntoExisting(ctx, src, stage, filesystem); err != nil {
		return fmt.Errorf("build directory copy staging area: %w", err)
	}
	if err := verifyCopiedSnapshot(ctx, stage, snapshot); err != nil {
		return fmt.Errorf("validate directory copy staging area: %w", err)
	}
	if err := verifySourceSnapshot(ctx, src, snapshot); err != nil {
		return fmt.Errorf("source changed while staging directory copy: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := filesystem.publish(stage, dst); err != nil {
		return fmt.Errorf("publish directory copy: %w", err)
	}
	published = true
	if err := syncDirectory(filepath.Dir(dst), filesystem); err != nil {
		return publishedMutation(fmt.Errorf("directory copy published but parent sync failed: %w", err))
	}
	return nil
}

func copyDirectoryIntoExisting(
	ctx context.Context,
	src, dst string,
	filesystem mutationFilesystem,
) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("copy directory source has mode %s", info.Mode())
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := copyPathNew(ctx, filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()), filesystem); err != nil {
			return err
		}
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	return syncDirectory(dst, filesystem)
}

func verifyCopiedSnapshot(ctx context.Context, destination string, expected sourceSnapshot) error {
	actual, err := captureSourceSnapshot(ctx, destination)
	if err != nil {
		return err
	}
	if len(expected.entries) != len(actual.entries) {
		return fmt.Errorf("entry set has %d objects, expected %d", len(actual.entries), len(expected.entries))
	}
	for index := range expected.entries {
		want, got := expected.entries[index], actual.entries[index]
		if want.relative != got.relative {
			return fmt.Errorf("entry set changed near %q", want.relative)
		}
		wantMode, gotMode := os.FileMode(want.entry.Mode), os.FileMode(got.entry.Mode)
		if want.entry.Kind != got.entry.Kind || wantMode.Perm() != gotMode.Perm() {
			return fmt.Errorf("entry %q kind or mode differs", want.relative)
		}
		switch want.entry.Kind {
		case objectKindDirectory:
			if !want.entry.ModTime.Equal(got.entry.ModTime) {
				return fmt.Errorf("directory %q modification time differs", want.relative)
			}
		case objectKindFile:
			if want.entry.Size != got.entry.Size || !want.entry.ModTime.Equal(got.entry.ModTime) {
				return fmt.Errorf("file %q size or modification time differs", want.relative)
			}
		case objectKindSymlink:
			if want.entry.Symlink != got.entry.Symlink {
				return fmt.Errorf("symlink %q target differs", want.relative)
			}
		default:
			return fmt.Errorf("entry %q has unsupported kind %q", want.relative, want.entry.Kind)
		}
	}
	return nil
}

func makeTreeRemovable(path string) {
	_ = filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(current, 0o700)
		}
		return nil
	})
}

func copyPathNew(ctx context.Context, src, dst string, filesystem mutationFilesystem) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		return copyDirectoryIntoExisting(ctx, src, dst, filesystem)
	case info.Mode().IsRegular():
		return copyRegular(ctx, src, dst, info, filesystem)
	default:
		return fmt.Errorf("cannot copy %s", info.Mode())
	}
}

func copyRegular(
	ctx context.Context,
	src, dst string,
	info fs.FileInfo,
	filesystem mutationFilesystem,
) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := filesystem.close(out); err == nil {
			err = closeErr
		}
		if err != nil {
			if removeErr := filesystem.remove(dst); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				err = ownedDestinationMutation(errors.Join(err, fmt.Errorf("remove incomplete copy: %w", removeErr)))
			}
		}
	}()
	written, copyErr := filesystem.copy(out, contextReader{ctx: ctx, reader: in})
	if copyErr != nil {
		err = copyErr
		return err
	}
	if written != info.Size() {
		return fmt.Errorf("short copy: wrote %d of %d bytes", written, info.Size())
	}
	if err = os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	return filesystem.sync(out)
}

func archivePathGuarded(
	ctx context.Context,
	src, dst, format string,
	policy ConflictPolicy,
	expected *fsinfo.PathExpectation,
	filesystem mutationFilesystem,
) error {
	if info, statErr := os.Lstat(src); statErr == nil && info.IsDir() && within(src, dst) {
		return fmt.Errorf("archive destination %q is inside source %q", dst, src)
	}
	return withDestinationGuarded(dst, policy, expected, filesystem, func() error {
		return archivePathNew(ctx, src, dst, format)
	})
}

func archivePathNew(ctx context.Context, src, dst, format string) (err error) {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if archiveFormat(dst, format) == archiveFormatZIP {
		return writeZip(ctx, f, src)
	}
	var w io.Writer = f
	var gz *gzip.Writer
	if archiveFormat(dst, format) == archiveFormatTarGZ {
		gz = gzip.NewWriter(f)
		w = gz
	}
	tw := tar.NewWriter(w)
	base := filepath.Dir(src)
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name, err = filepath.Rel(base, path)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(h.Name)
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		_, cpErr := io.Copy(tw, contextReader{ctx: ctx, reader: in})
		closeErr := in.Close()
		if cpErr != nil {
			return cpErr
		}
		return closeErr
	})
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if gz != nil {
		if closeErr := gz.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

type archiveEntryKind uint8

const (
	archiveEntryDirectory archiveEntryKind = iota + 1
	archiveEntryRegular
	archiveEntrySymlink
)

type archiveEntrySpec struct {
	name     string
	kind     archiveEntryKind
	mode     os.FileMode
	linkname string
}

type archiveDirectorySpec struct {
	name string
	mode os.FileMode
}

// archiveLayout is the single deterministic policy model used by dry-run and
// real extraction. It rejects entries whose path depends on a link or other
// non-directory entry, irrespective of archive order. Actual writes are still
// rooted with os.Root so a filesystem race cannot redirect them outside the
// staging root.
type archiveLayout struct {
	entries     map[string]archiveEntrySpec
	order       []archiveEntrySpec
	directories map[string]archiveDirectorySpec
}

func newArchiveLayout() *archiveLayout {
	return &archiveLayout{
		entries:     make(map[string]archiveEntrySpec),
		directories: make(map[string]archiveDirectorySpec),
	}
}

func (l *archiveLayout) add(spec archiveEntrySpec) error {
	normalized, err := normalizeArchiveEntry(spec)
	if err != nil {
		return err
	}
	key := archivePathKey(normalized.name)
	if _, exists := l.entries[key]; exists {
		return fmt.Errorf("duplicate archive path %q", normalized.name)
	}

	for parent := filepath.Dir(normalized.name); parent != "."; parent = filepath.Dir(parent) {
		if entry, exists := l.entries[archivePathKey(parent)]; exists && entry.kind != archiveEntryDirectory {
			return fmt.Errorf("archive path %q has non-directory parent %q", normalized.name, parent)
		}
		l.rememberDirectory(parent, 0o755, false)
	}
	if normalized.kind != archiveEntryDirectory {
		prefix := key + string(filepath.Separator)
		for existingKey := range l.entries {
			if strings.HasPrefix(existingKey, prefix) {
				return fmt.Errorf("non-directory archive entry %q has descendant entries", normalized.name)
			}
		}
	} else {
		l.rememberDirectory(normalized.name, normalized.mode.Perm(), true)
	}

	l.entries[key] = normalized
	l.order = append(l.order, normalized)
	return nil
}

func (l *archiveLayout) rememberDirectory(name string, mode os.FileMode, explicit bool) {
	key := archivePathKey(name)
	if existing, ok := l.directories[key]; ok && !explicit {
		return
	} else if ok && explicit {
		existing.mode = mode
		l.directories[key] = existing
		return
	}
	l.directories[key] = archiveDirectorySpec{name: name, mode: mode}
}

func (l *archiveLayout) match(spec archiveEntrySpec, seen map[string]bool) (archiveEntrySpec, error) {
	normalized, err := normalizeArchiveEntry(spec)
	if err != nil {
		return archiveEntrySpec{}, err
	}
	key := archivePathKey(normalized.name)
	if seen[key] {
		return archiveEntrySpec{}, fmt.Errorf("archive changed during extraction: duplicate entry %q", normalized.name)
	}
	expected, ok := l.entries[key]
	if !ok || expected.kind != normalized.kind || expected.mode.Perm() != normalized.mode.Perm() || expected.linkname != normalized.linkname {
		return archiveEntrySpec{}, fmt.Errorf("archive changed during extraction at %q", normalized.name)
	}
	seen[key] = true
	return expected, nil
}

func (l *archiveLayout) complete(seen map[string]bool) error {
	if len(seen) != len(l.entries) {
		return errors.New("archive changed during extraction: entries are missing")
	}
	return nil
}

func archivePathKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == windowsOS {
		return strings.ToLower(path)
	}
	return path
}

func normalizeArchiveEntry(spec archiveEntrySpec) (archiveEntrySpec, error) {
	name, err := cleanArchiveName(spec.name)
	if err != nil {
		return archiveEntrySpec{}, err
	}
	spec.name = name
	spec.mode = spec.mode.Perm()
	if spec.kind != archiveEntrySymlink {
		spec.linkname = ""
		return spec, nil
	}
	if spec.linkname == "" || strings.IndexByte(spec.linkname, 0) >= 0 {
		return archiveEntrySpec{}, fmt.Errorf("unsafe symlink target %q in %q", spec.linkname, spec.name)
	}
	target := filepath.FromSlash(spec.linkname)
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return archiveEntrySpec{}, fmt.Errorf("unsafe symlink target %q in %q", spec.linkname, spec.name)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(spec.name), target))
	if !filepath.IsLocal(resolved) {
		return archiveEntrySpec{}, fmt.Errorf("unsafe symlink target %q in %q", spec.linkname, spec.name)
	}
	spec.linkname = target
	return spec, nil
}

func cleanArchiveName(name string) (string, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || !filepath.IsLocal(clean) {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	return clean, nil
}

func tarArchiveEntry(header *tar.Header) (archiveEntrySpec, error) {
	spec := archiveEntrySpec{name: header.Name, mode: os.FileMode(header.Mode)}
	switch header.Typeflag {
	case tar.TypeDir:
		spec.kind = archiveEntryDirectory
	case tar.TypeReg, 0:
		spec.kind = archiveEntryRegular
	case tar.TypeSymlink:
		spec.kind, spec.linkname = archiveEntrySymlink, header.Linkname
	case tar.TypeLink:
		return archiveEntrySpec{}, fmt.Errorf("archive hardlink %q is not supported", header.Name)
	default:
		return archiveEntrySpec{}, fmt.Errorf("unsupported archive entry type %d in %q", header.Typeflag, header.Name)
	}
	return spec, nil
}

func zipArchiveEntry(entry *zip.File, linkname string) (archiveEntrySpec, error) {
	mode := entry.Mode()
	spec := archiveEntrySpec{name: entry.Name, mode: mode}
	switch {
	case mode.IsDir():
		spec.kind = archiveEntryDirectory
	case mode&os.ModeSymlink != 0:
		spec.kind, spec.linkname = archiveEntrySymlink, linkname
	case mode.IsRegular():
		spec.kind = archiveEntryRegular
	default:
		return archiveEntrySpec{}, fmt.Errorf("unsupported zip entry mode %s in %q", mode, entry.Name)
	}
	return spec, nil
}

func inspectArchive(ctx context.Context, src, format string) (*archiveLayout, error) {
	if archiveFormat(src, format) == archiveFormatZIP {
		return inspectZipArchive(ctx, src)
	}
	return inspectTarArchive(ctx, src, format)
}

func inspectTarArchive(ctx context.Context, src, format string) (*archiveLayout, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if archiveFormat(src, format) == archiveFormatTarGZ {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}

	layout := newArchiveLayout()
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, err := tr.Next()
		if err == io.EOF {
			return layout, nil
		}
		if err != nil {
			return nil, err
		}
		spec, err := tarArchiveEntry(header)
		if err != nil {
			return nil, err
		}
		if err := layout.add(spec); err != nil {
			return nil, err
		}
		if _, err := io.Copy(io.Discard, contextReader{ctx: ctx, reader: tr}); err != nil {
			return nil, err
		}
	}
}

func inspectZipArchive(ctx context.Context, src string) (*archiveLayout, error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	layout := newArchiveLayout()
	for _, entry := range zr.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		linkname := ""
		if entry.Mode()&os.ModeSymlink != 0 {
			linkname, err = readZipSymlink(ctx, entry)
		} else {
			err = consumeZipEntry(ctx, entry, io.Discard)
		}
		if err != nil {
			return nil, err
		}
		spec, err := zipArchiveEntry(entry, linkname)
		if err != nil {
			return nil, err
		}
		if err := layout.add(spec); err != nil {
			return nil, err
		}
	}
	return layout, nil
}

func consumeZipEntry(ctx context.Context, entry *zip.File, dst io.Writer) error {
	in, err := entry.Open()
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, contextReader{ctx: ctx, reader: in})
	closeErr := in.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func readZipSymlink(ctx context.Context, entry *zip.File) (string, error) {
	in, err := entry.Open()
	if err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: in}, 4097))
	closeErr := in.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if len(data) > 4096 {
		return "", fmt.Errorf("symlink target too long in %q", entry.Name)
	}
	return string(data), nil
}

func extractArchiveGuarded(
	ctx context.Context,
	src, dst, format string,
	policy ConflictPolicy,
	layout *archiveLayout,
	expected *fsinfo.PathExpectation,
	filesystem mutationFilesystem,
) error {
	if layout == nil {
		return errors.New("archive was not validated")
	}
	return withDestinationGuarded(dst, policy, expected, filesystem, func() error {
		return extractArchiveNew(ctx, src, dst, format, layout)
	})
}

func extractArchiveNew(ctx context.Context, src, dst, format string, layout *archiveLayout) error {
	staging, err := os.MkdirTemp(filepath.Dir(dst), ".dirstat-extract-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()

	root, err := os.OpenRoot(staging)
	if err != nil {
		return err
	}
	if archiveFormat(src, format) == archiveFormatZIP {
		err = extractZipArchive(ctx, src, root, layout)
	} else {
		err = extractTarArchive(ctx, src, format, root, layout)
	}
	if err == nil {
		err = materializeArchiveSymlinks(ctx, root, layout)
	}
	if err == nil {
		err = applyArchiveDirectoryModes(ctx, root, layout)
	}
	if err == nil {
		err = root.Chmod(".", 0o755)
	}
	closeErr := root.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(staging, dst); err != nil {
		return err
	}
	published = true
	return nil
}

func extractTarArchive(ctx context.Context, src, format string, root *os.Root, layout *archiveLayout) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if archiveFormat(src, format) == archiveFormatTarGZ {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}

	seen := make(map[string]bool, len(layout.entries))
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if err == io.EOF {
			return layout.complete(seen)
		}
		if err != nil {
			return err
		}
		spec, err := tarArchiveEntry(header)
		if err != nil {
			return err
		}
		spec, err = layout.match(spec, seen)
		if err != nil {
			return err
		}
		switch spec.kind {
		case archiveEntryDirectory:
			if err := root.MkdirAll(spec.name, 0o700); err != nil {
				return err
			}
			_, err = io.Copy(io.Discard, contextReader{ctx: ctx, reader: tr})
		case archiveEntryRegular:
			err = writeArchiveFile(ctx, root, spec, tr)
		case archiveEntrySymlink:
			_, err = io.Copy(io.Discard, contextReader{ctx: ctx, reader: tr})
		}
		if err != nil {
			return err
		}
	}
}

func extractZipArchive(ctx context.Context, src string, root *os.Root, layout *archiveLayout) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	seen := make(map[string]bool, len(layout.entries))
	for _, entry := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		linkname := ""
		if entry.Mode()&os.ModeSymlink != 0 {
			linkname, err = readZipSymlink(ctx, entry)
			if err != nil {
				return err
			}
		}
		spec, err := zipArchiveEntry(entry, linkname)
		if err != nil {
			return err
		}
		spec, err = layout.match(spec, seen)
		if err != nil {
			return err
		}
		switch spec.kind {
		case archiveEntryDirectory:
			if err := root.MkdirAll(spec.name, 0o700); err != nil {
				return err
			}
			err = consumeZipEntry(ctx, entry, io.Discard)
		case archiveEntryRegular:
			in, openErr := entry.Open()
			if openErr != nil {
				return openErr
			}
			err = writeArchiveFile(ctx, root, spec, in)
			closeErr := in.Close()
			if err == nil {
				err = closeErr
			}
		case archiveEntrySymlink:
			// readZipSymlink already consumed and validated the entry.
		}
		if err != nil {
			return err
		}
	}
	return layout.complete(seen)
}

func writeArchiveFile(ctx context.Context, root *os.Root, spec archiveEntrySpec, src io.Reader) (err error) {
	parent := filepath.Dir(spec.name)
	if parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return err
		}
	}
	out, err := root.OpenFile(spec.name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, spec.mode.Perm())
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = root.Remove(spec.name)
		}
	}()
	_, err = io.Copy(out, contextReader{ctx: ctx, reader: src})
	return err
}

func materializeArchiveSymlinks(ctx context.Context, root *os.Root, layout *archiveLayout) error {
	for _, spec := range layout.order {
		if spec.kind != archiveEntrySymlink {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		parent := filepath.Dir(spec.name)
		if parent != "." {
			if err := root.MkdirAll(parent, 0o700); err != nil {
				return err
			}
		}
		if err := root.Symlink(spec.linkname, spec.name); err != nil {
			return err
		}
	}
	return nil
}

func applyArchiveDirectoryModes(ctx context.Context, root *os.Root, layout *archiveLayout) error {
	directories := make([]archiveDirectorySpec, 0, len(layout.directories))
	for _, directory := range layout.directories {
		directories = append(directories, directory)
	}
	sort.Slice(directories, func(i, j int) bool {
		left := strings.Count(directories[i].name, string(filepath.Separator))
		right := strings.Count(directories[j].name, string(filepath.Separator))
		if left == right {
			return directories[i].name < directories[j].name
		}
		return left > right
	})
	for _, directory := range directories {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Chmod(directory.name, directory.mode.Perm()); err != nil {
			return err
		}
	}
	return nil
}

func archiveFormat(path, format string) string {
	format = strings.ToLower(format)
	if format == "tgz" || format == "gzip" {
		return archiveFormatTarGZ
	}
	if format == archiveFormatTar || format == archiveFormatTarGZ || format == archiveFormatZIP {
		return format
	}
	if format != "" {
		return "invalid"
	}
	if strings.HasSuffix(strings.ToLower(path), ".tgz") || strings.HasSuffix(strings.ToLower(path), ".tar.gz") {
		return archiveFormatTarGZ
	}
	if strings.HasSuffix(strings.ToLower(path), ".zip") {
		return archiveFormatZIP
	}
	return archiveFormatTar
}

func writeZip(ctx context.Context, out io.Writer, src string) (err error) {
	zw := zip.NewWriter(out)
	base := filepath.Dir(src)
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name, err = filepath.Rel(base, path)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(h.Name)
		if info.IsDir() {
			h.Name += "/"
		}
		h.Method = zip.Deflate
		entry, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, err = io.WriteString(entry, target)
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		_, cpErr := io.Copy(entry, contextReader{ctx: ctx, reader: in})
		closeErr := in.Close()
		if cpErr != nil {
			return cpErr
		}
		return closeErr
	})
	if closeErr := zw.Close(); err == nil {
		err = closeErr
	}
	return err
}
