package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	mirrorRetryFloor   = 5 * time.Second
	mirrorPollInterval = 5 * time.Second
)

var errMirrorDeferred = errors.New("mirror deferred to newer revision")

// MirrorStore is the durable boundary between authoritative workstream data
// and the one-way archive. Implementations must never perform filesystem I/O
// while scheduling or acknowledging mirror state.
type MirrorStore interface {
	NextPendingMirror(context.Context, time.Time) (MirrorStateView, bool, error)
	RenderMirrorSnapshot(context.Context, MirrorKey, int64) (MirrorDocument, error)
	AckMirrorWrite(context.Context, MirrorKey, int64, string, time.Time) error
	RecordMirrorFailure(context.Context, MirrorKey, int64, string, bool, time.Time) error
	RetryMirror(context.Context, MirrorKey, time.Time) error
}

type MirrorWorkerConfig struct {
	// Root is an already-resolved configured vault directory. The worker does
	// not infer a home directory, which keeps handler tests and disabled-mirror
	// configurations away from a user's actual vault.
	Root string
	Now  func() time.Time
}

// MirrorWorker serializes all vault changes. Database state is durable, so a
// stopped worker only delays an export; it never rolls back a local command.
type MirrorWorker struct {
	store MirrorStore
	root  string
	now   func() time.Time

	mu     sync.Mutex // serializes Run/Drain callers into one filesystem writer
	wakeCh chan struct{}
}

// NewMirrorWorker deliberately accepts an inaccessible vault. A malformed or
// unavailable vault is reported per pending entity during Drain, while Cockpit
// commands remain usable and durable in SQLite.
func NewMirrorWorker(store MirrorStore, cfg MirrorWorkerConfig) *MirrorWorker {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &MirrorWorker{store: store, root: cfg.Root, now: now, wakeCh: make(chan struct{}, 1)}
}

// Wake requests prompt processing after a committed semantic mutation. It is
// intentionally non-blocking: durable rows make a dropped duplicate wake safe.
func (w *MirrorWorker) Wake() {
	select {
	case w.wakeCh <- struct{}{}:
	default:
	}
}

// Run wakes promptly after commits and polls at the retry floor so durable
// retry deadlines are honored even when no later command arrives.
func (w *MirrorWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(mirrorPollInterval)
	defer ticker.Stop()
	w.Wake()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wakeCh:
		case <-ticker.C:
		}
		if err := w.Drain(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("drain mirror", "err", err)
		}
	}
}

// Drain synchronously handles all currently eligible durable work. It is the
// deterministic public control used by HTTP tests with temporary vaults.
func (w *MirrorWorker) Drain(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.store == nil {
		return errors.New("mirror store is unavailable")
	}
	for {
		state, ok, err := w.store.NextPendingMirror(ctx, w.now())
		if err != nil || !ok {
			return err
		}
		if err := w.process(ctx, state); err != nil {
			if errors.Is(err, errMirrorDeferred) {
				return nil
			}
			return err
		}
	}
}

// Retry schedules a normal durable retry. It never grants overwrite authority
// to a file in conflict.
func (w *MirrorWorker) Retry(ctx context.Context, key MirrorKey) error {
	if w.store == nil {
		return errors.New("mirror store is unavailable")
	}
	if !validMirrorKey(key) {
		return &ValidationError{Fields: map[string]string{"mirror": "unsupported mirror entity"}}
	}
	if err := w.store.RetryMirror(ctx, key, w.now()); err != nil {
		return err
	}
	w.Wake()
	return nil
}

func (w *MirrorWorker) process(ctx context.Context, state MirrorStateView) error {
	if !validMirrorKey(state.MirrorKey) {
		return w.recordFailure(ctx, state, state.DesiredRevision, &mirrorConflictError{reason: "unsupported mirror entity"}, true)
	}
	doc, err := w.store.RenderMirrorSnapshot(ctx, state.MirrorKey, state.DesiredRevision)
	if errors.Is(err, ErrRevisionUnavailable) {
		// A correctly scheduled newer revision will be picked up on the next
		// durable pass. Record this one as retryable so a missing/corrupt row
		// cannot spin forever and starve later pending exports.
		return w.recordFailure(ctx, state, state.DesiredRevision, err, false)
	}
	if err != nil {
		return w.recordFailure(ctx, state, state.DesiredRevision, err, false)
	}
	if doc.Key != state.MirrorKey || doc.Revision != state.DesiredRevision {
		return w.recordFailure(ctx, state, state.DesiredRevision, errors.New("incoherent mirror snapshot"), false)
	}
	contents, err := formatMirrorDocument(doc)
	if err != nil {
		return w.recordFailure(ctx, state, doc.Revision, err, false)
	}
	checksum := checksum(contents)
	if err := w.writeDocument(doc.RelativePath, contents, state.LastChecksum); err != nil {
		var conflict *mirrorConflictError
		if errors.As(err, &conflict) {
			return w.recordFailure(ctx, state, doc.Revision, err, true)
		}
		return w.recordFailure(ctx, state, doc.Revision, err, false)
	}
	if err := w.store.AckMirrorWrite(ctx, state.MirrorKey, doc.Revision, checksum, w.now()); err != nil {
		return fmt.Errorf("ack mirror write: %w", err)
	}
	return nil
}

func validMirrorKey(key MirrorKey) bool {
	_, ok := mirrorDirectory(key.EntityType)
	return ok && validOpaqueID(key.EntityID)
}

func (w *MirrorWorker) recordFailure(ctx context.Context, state MirrorStateView, revision int64, cause error, conflict bool) error {
	// The durable store owns retry calculation and transition deduplication.
	if err := w.store.RecordMirrorFailure(ctx, state.MirrorKey, revision, mirrorErrorMessage(cause), conflict, w.now()); err != nil {
		return fmt.Errorf("record mirror failure: %w", err)
	}
	if conflict && state.Status != "conflict" {
		slog.Warn("mirror conflict", "entity_type", state.EntityType, "entity_id", state.EntityID, "revision", revision)
	} else if !conflict && state.Status != "error" {
		slog.Warn("mirror write failed", "entity_type", state.EntityType, "entity_id", state.EntityID, "revision", revision)
	}
	return nil
}

// mirrorConflictError means the worker deliberately left a user-managed file
// untouched. Retrying alone remains a conflict, never a force overwrite.
type mirrorConflictError struct{ reason string }

func (e *mirrorConflictError) Error() string { return e.reason }

func mirrorErrorMessage(err error) string {
	var conflict *mirrorConflictError
	if errors.As(err, &conflict) {
		return conflict.reason
	}
	return "vault write failed"
}

func formatMirrorDocument(doc MirrorDocument) ([]byte, error) {
	if err := validateMirrorPath(doc.Key, doc.RelativePath); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("---\n")
	keys := make([]string, 0, len(doc.Frontmatter))
	for key := range doc.Frontmatter {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !validFrontmatterKey(key) {
			return nil, fmt.Errorf("invalid frontmatter key")
		}
		b.WriteString(key)
		b.WriteString(": ")
		// JSON double-quoted scalars are valid YAML 1.2 scalars and avoid
		// frontmatter injection without a YAML dependency.
		b.WriteString(strconv.Quote(doc.Frontmatter[key]))
		b.WriteByte('\n')
	}
	b.WriteString("---\n\n# ")
	b.WriteString(doc.Title)
	b.WriteString("\n\n")
	b.WriteString(doc.Body)
	if len(doc.Links) > 0 {
		b.WriteString("\n\n## Related\n")
		for _, link := range doc.Links {
			if !validRelativeMirrorLink(doc.RelativePath, link.RelativePath) {
				return nil, errors.New("invalid relative mirror link")
			}
			b.WriteString("- [")
			b.WriteString(escapeMarkdownLabel(link.Label))
			b.WriteString("](")
			b.WriteString(link.RelativePath)
			b.WriteString(")\n")
		}
	}
	return []byte(b.String()), nil
}

func validRelativeMirrorLink(from, link string) bool {
	if link == "" || path.IsAbs(link) {
		return false
	}
	// A relative Markdown link may legitimately begin with ../. It still must
	// resolve to one of the fixed managed paths, never above the vault root.
	target := path.Clean(path.Join(path.Dir(from), link))
	if !fs.ValidPath(target) {
		return false
	}
	parts := strings.Split(target, "/")
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".md") {
		return false
	}
	name := strings.TrimSuffix(parts[1], ".md")
	if !validOpaqueID(name) {
		return false
	}
	for _, dir := range []string{"workstreams", "engineering", "asks", "signals", "decisions"} {
		if parts[0] == dir {
			return true
		}
	}
	return false
}

func validateMirrorPath(key MirrorKey, relative string) error {
	if key.EntityID == "" || !validOpaqueID(key.EntityID) {
		return errors.New("invalid mirror entity id")
	}
	dir, ok := mirrorDirectory(key.EntityType)
	if !ok || relative != dir+"/"+key.EntityID+".md" || !fs.ValidPath(relative) {
		return errors.New("invalid mirror path")
	}
	return nil
}

func mirrorDirectory(entityType string) (string, bool) {
	switch entityType {
	case "workstream":
		return "workstreams", true
	case "engineering_item":
		return "engineering", true
	case "ask":
		return "asks", true
	case "signal":
		return "signals", true
	case "decision":
		return "decisions", true
	default:
		return "", false
	}
}

func validOpaqueID(id string) bool {
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func validFrontmatterKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func escapeMarkdownLabel(s string) string {
	return strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "\n", " ").Replace(s)
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (w *MirrorWorker) writeDocument(relative string, contents []byte, knownChecksum string) error {
	root, err := w.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := ensureMirrorParent(root, relative); err != nil {
		return err
	}
	if err := checkMirrorTarget(root, relative, knownChecksum); err != nil {
		return err
	}

	tmp, err := mirrorTemporaryName(relative, "tmp")
	if err != nil {
		return err
	}
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary mirror file: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = root.Remove(tmp)
		}
	}()
	if _, err := f.Write(contents); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temporary mirror file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync temporary mirror file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary mirror file: %w", err)
	}

	// Check immediately before atomic replacement. os.Root confines every
	// path and rejects escaping symlinks; no portable standard-library API can
	// also provide hostile-writer compare-and-swap for Rename.
	if err := checkMirrorTarget(root, relative, knownChecksum); err != nil {
		return err
	}
	backup := ""
	if knownChecksum != "" {
		// Keep a same-filesystem hard-link to the expected old bytes while
		// replacing them. This is best-effort: filesystems without hard-link
		// support still get the atomic rename path, while a detected external
		// change is retained instead of silently discarded.
		if candidate, err := mirrorTemporaryName(relative, "bak"); err == nil {
			if err := root.Link(relative, candidate); err == nil {
				backup = candidate
				if err := checkMirrorTarget(root, backup, knownChecksum); err != nil {
					return err // preserve the suspect backup for reconciliation
				}
			}
		}
	}
	if err := root.Rename(tmp, relative); err != nil {
		return fmt.Errorf("replace mirror file: %w", err)
	}
	cleanup = false
	if backup != "" {
		if err := checkMirrorTarget(root, backup, knownChecksum); err != nil {
			return err // an edit racing the replacement remains in the backup
		}
		_ = root.Remove(backup)
	}
	return nil
}

func (w *MirrorWorker) openRoot() (*os.Root, error) {
	if w.root == "" {
		return nil, errors.New("mirror root is not configured")
	}
	if err := os.MkdirAll(w.root, 0o700); err != nil {
		return nil, fmt.Errorf("prepare mirror root: %w", err)
	}
	info, err := os.Lstat(w.root)
	if err != nil {
		return nil, fmt.Errorf("inspect mirror root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, &mirrorConflictError{reason: "configured vault is not a regular directory"}
	}
	root, err := os.OpenRoot(w.root)
	if err != nil {
		return nil, fmt.Errorf("open mirror root: %w", err)
	}
	return root, nil
}

func ensureMirrorParent(root *os.Root, relative string) error {
	dir := path.Dir(relative)
	if dir == "." || !fs.ValidPath(dir) {
		return &mirrorConflictError{reason: "invalid managed mirror directory"}
	}
	current := ""
	for _, component := range strings.Split(dir, "/") {
		if current == "" {
			current = component
		} else {
			current += "/" + component
		}
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(current, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("create managed mirror directory: %w", err)
			}
			info, err = root.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("inspect managed mirror directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return &mirrorConflictError{reason: "managed mirror directory is not a regular directory"}
		}
	}
	return nil
}

func checkMirrorTarget(root *os.Root, relative, knownChecksum string) error {
	info, err := root.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		if knownChecksum != "" {
			return &mirrorConflictError{reason: "previously mirrored file is missing"}
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect mirror target: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return &mirrorConflictError{reason: "mirror target is not a regular file"}
	}
	if knownChecksum == "" {
		return &mirrorConflictError{reason: "unexpected file occupies mirror target"}
	}
	data, err := root.ReadFile(relative)
	if err != nil {
		return fmt.Errorf("read mirror target: %w", err)
	}
	if checksum(data) != knownChecksum {
		return &mirrorConflictError{reason: "mirror target was edited outside Cockpit"}
	}
	return nil
}

func mirrorTemporaryName(relative, suffix string) (string, error) {
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate temporary mirror name: %w", err)
	}
	return path.Dir(relative) + "/." + path.Base(relative) + "." + hex.EncodeToString(token[:]) + "." + suffix, nil
}
