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
	"strconv"
	"strings"
	"time"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

const (
	archiveFormatZIP   = "zip"
	archiveFormatTarGZ = "tar.gz"
)

func DefaultAuditPath(root string) string { return filepath.Join(root, ".dirstat-audit.jsonl") }

func OpenAudit(path string) (*os.File, error) {
	f, err := openAuditFile(path)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func applyPlan(ctx context.Context, plan Plan, opts ApplyOptions) ([]Result, error) {
	if plan.Header.Version != PlanVersion {
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
	audit := opts.Audit
	var owned *os.File
	if audit == nil && !opts.DisableAudit {
		path := opts.AuditPath
		if path == "" {
			path = DefaultAuditPath(root)
		}
		owned, err = OpenAudit(path)
		if err != nil {
			return nil, fmt.Errorf("open audit log: %w", err)
		}
		audit = owned
		defer func() { _ = owned.Close() }()
	}

	results := make([]Result, 0, len(plan.Operations))
	created := make(map[string]bool)
	for _, op := range plan.Operations {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		started := time.Now().UTC()
		r := Result{Type: "result", Version: PlanVersion, OperationID: op.ID, Action: op.Action, Status: "ok", DryRun: opts.DryRun, StartedAt: started}
		sourceKey := op.Source
		if !filepath.IsAbs(sourceKey) {
			sourceKey = filepath.Join(root, sourceKey)
		}
		sourceKey, _ = filepath.Abs(filepath.Clean(sourceKey))
		err := execute(ctx, root, op, opts.DryRun, policy, opts.AllowUnguarded || created[sourceKey])
		if err != nil {
			r.Status, r.Error = "error", err.Error()
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
		if audit != nil {
			if writeErr := WriteResult(audit, r); writeErr != nil {
				return results, fmt.Errorf("write audit result: %w", writeErr)
			}
		}
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

func execute(ctx context.Context, root string, op Operation, dry bool, policy ConflictPolicy, allowUnguarded bool) error {
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

	switch op.Action {
	case ActionDelete:
		if op.Recursive {
			return os.RemoveAll(src)
		}
		return os.Remove(src)
	case ActionCopy:
		return copyPath(ctx, src, dst, policy)
	case ActionMove, ActionRename:
		return movePath(ctx, src, dst, policy)
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
		return archivePath(ctx, src, dst, op.Format, policy)
	case ActionExtract:
		if err := rejectSymlink(src); err != nil {
			return err
		}
		return extractArchive(ctx, src, dst, op.Format, policy)
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
		if err := validateArchive(ctx, src, op.Format); err != nil {
			return err
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
	if runtime.GOOS == "windows" {
		root, path = strings.ToLower(root), strings.ToLower(path)
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
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
func withDestination(dst string, policy ConflictPolicy, build func() error) error {
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
		_ = os.Remove(backup)
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	if err := os.Rename(dst, backup); err != nil {
		return err
	}
	if err := build(); err != nil {
		_ = os.RemoveAll(dst)
		if restoreErr := os.Rename(backup, dst); restoreErr != nil {
			return fmt.Errorf("%w (also failed to restore destination: %v)", err, restoreErr)
		}
		return err
	}
	return os.RemoveAll(backup)
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

func movePath(ctx context.Context, src, dst string, policy ConflictPolicy) error {
	return withDestination(dst, policy, func() error { return movePathNew(ctx, src, dst) })
}

func movePathNew(ctx context.Context, src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !isCrossDevice(err) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parent := filepath.Dir(dst)
	tmp, err := os.MkdirTemp(parent, ".dirstat-move-")
	if err != nil {
		return err
	}
	_ = os.Remove(tmp)
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := copyPath(ctx, src, tmp, ConflictFail); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func copyPath(ctx context.Context, src, dst string, policy ConflictPolicy) error {
	if info, err := os.Lstat(src); err == nil && info.IsDir() && within(src, dst) {
		return fmt.Errorf("destination %q is inside source %q", dst, src)
	}
	return withDestination(dst, policy, func() error { return copyPathNew(ctx, src, dst) })
}

func copyPathNew(ctx context.Context, src, dst string) error {
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
		if err := os.Mkdir(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := copyPath(ctx, filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()), ConflictFail); err != nil {
				return err
			}
		}
		return os.Chtimes(dst, info.ModTime(), info.ModTime())
	case info.Mode().IsRegular():
		return copyRegular(ctx, src, dst, info)
	default:
		return fmt.Errorf("cannot copy %s", info.Mode())
	}
}

func copyRegular(ctx context.Context, src, dst string, info fs.FileInfo) (err error) {
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
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if _, err = io.Copy(out, contextReader{ctx: ctx, reader: in}); err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

func archivePath(ctx context.Context, src, dst, format string, policy ConflictPolicy) error {
	if info, statErr := os.Lstat(src); statErr == nil && info.IsDir() && within(src, dst) {
		return fmt.Errorf("archive destination %q is inside source %q", dst, src)
	}
	return withDestination(dst, policy, func() error { return archivePathNew(ctx, src, dst, format) })
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

func extractArchive(ctx context.Context, src, dst, format string, policy ConflictPolicy) error {
	return withDestination(dst, policy, func() error { return extractArchiveNew(ctx, src, dst, format) })
}

func validateArchive(ctx context.Context, src, format string) error {
	if archiveFormat(src, format) == archiveFormatZIP {
		zr, err := zip.OpenReader(src)
		if err != nil {
			return err
		}
		defer func() { _ = zr.Close() }()
		for _, entry := range zr.File {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := validateArchiveName(entry.Name); err != nil {
				return err
			}
			r, err := entry.Open()
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(io.Discard, contextReader{ctx: ctx, reader: r})
			closeErr := r.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	}
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
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := validateArchiveName(header.Name); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, contextReader{ctx: ctx, reader: tr}); err != nil {
			return err
		}
	}
}

func validateArchiveName(name string) error {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe archive path %q", name)
	}
	return nil
}

func extractArchiveNew(ctx context.Context, src, dst, format string) (err error) {
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dst)
		}
	}()
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if archiveFormat(src, format) == archiveFormatZIP {
		if err := f.Close(); err != nil {
			return err
		}
		return extractZip(ctx, src, dst)
	}
	var r io.Reader = f
	if archiveFormat(src, format) == archiveFormatTarGZ {
		gz, e := gzip.NewReader(f)
		if e != nil {
			return e
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if err := validateArchiveName(h.Name); err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(h.Name))
		path := filepath.Join(dst, clean)
		if !within(dst, path) {
			return fmt.Errorf("unsafe archive path %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, os.FileMode(h.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			out, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode).Perm())
			if e != nil {
				return e
			}
			_, cpErr := io.Copy(out, contextReader{ctx: ctx, reader: tr})
			closeErr := out.Close()
			if cpErr != nil {
				return cpErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(h.Linkname) {
				return fmt.Errorf("unsafe absolute symlink %q", h.Linkname)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(h.Linkname)))
			if !within(dst, resolved) {
				return fmt.Errorf("unsafe symlink %q", h.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type %s", strconv.Itoa(int(h.Typeflag)))
		}
	}
	return nil
}

func archiveFormat(path, format string) string {
	format = strings.ToLower(format)
	if format == "tgz" || format == "gzip" {
		return archiveFormatTarGZ
	}
	if format == "tar" || format == archiveFormatTarGZ || format == archiveFormatZIP {
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
	return "tar"
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

func extractZip(ctx context.Context, src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	for _, entry := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(entry.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path %q", entry.Name)
		}
		path := filepath.Join(dst, clean)
		if !within(dst, path) {
			return fmt.Errorf("unsafe archive path %q", entry.Name)
		}
		mode := entry.Mode()
		if mode.IsDir() {
			if err := os.MkdirAll(path, mode.Perm()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		in, err := entry.Open()
		if err != nil {
			return err
		}
		if mode&os.ModeSymlink != 0 {
			data, readErr := io.ReadAll(io.LimitReader(in, 4097))
			closeErr := in.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			if len(data) > 4096 {
				return fmt.Errorf("symlink target too long in %q", entry.Name)
			}
			target := string(data)
			if filepath.IsAbs(target) || !within(dst, filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target)))) {
				return fmt.Errorf("unsafe symlink %q", target)
			}
			if err := os.Symlink(target, path); err != nil {
				return err
			}
			continue
		}
		if !mode.IsRegular() {
			_ = in.Close()
			return fmt.Errorf("unsupported zip entry mode %s", mode)
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
		if err != nil {
			_ = in.Close()
			return err
		}
		_, cpErr := io.Copy(out, contextReader{ctx: ctx, reader: in})
		inErr := in.Close()
		outErr := out.Close()
		if cpErr != nil {
			return cpErr
		}
		if inErr != nil {
			return inErr
		}
		if outErr != nil {
			return outErr
		}
	}
	return nil
}
