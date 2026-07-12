package hop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	defaultGitBinary  = "git"
	defaultCommitName = "Hop"
	defaultCommitMail = "hop@localhost"
	hiddenRefPrefix   = "refs/hop/"
)

// GitIdentity is the identity written to synthetic commits made by Hop.
type GitIdentity struct {
	Name  string
	Email string
}

// GitStore locates repositories and supplies the Git executable and synthetic
// commit identity used by them. Its zero value is ready to use.
type GitStore struct {
	Binary   string
	Identity GitIdentity
	Now      func() time.Time
}

// Repository is a non-bare Git worktree. Hop uses the repository's object
// database as storage, but never needs to stage data in the user's index.
type Repository struct {
	root         string
	gitDir       string
	commonGitDir string
	store        *GitStore
}

// GitError reports a failed Git invocation without exposing its environment.
type GitError struct {
	Args     []string
	ExitCode int
	Stdout   string
	Stderr   string
	Err      error
}

func (e *GitError) Error() string {
	message := strings.TrimSpace(e.Stderr)
	if message == "" {
		message = strings.TrimSpace(e.Stdout)
	}
	if message == "" {
		message = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), message)
}

func (e *GitError) Unwrap() error { return e.Err }

// CommitOptions controls a synthetic commit. Empty identities and timestamps
// use the GitStore defaults.
type CommitOptions struct {
	Message       string
	Parents       []string
	Author        GitIdentity
	Committer     GitIdentity
	AuthorTime    time.Time
	CommitterTime time.Time
}

// SnapshotOptions controls a workspace snapshot commit.
type SnapshotOptions struct {
	Message string
	Commit  CommitOptions
}

// PathChange is a rename-aware path-level Git change. For renames and copies,
// OldPath and NewPath are both populated. Other changes use NewPath.
type PathChange struct {
	Status  string `json:"status"`
	OldPath string `json:"old_path,omitempty"`
	NewPath string `json:"new_path,omitempty"`
}

// NewGitStore returns a Git store with Hop's controlled commit identity.
func NewGitStore() *GitStore {
	return &GitStore{
		Binary: defaultGitBinary,
		Identity: GitIdentity{
			Name:  defaultCommitName,
			Email: defaultCommitMail,
		},
		Now: time.Now,
	}
}

// EnsureRepository opens the repository containing path, or initializes one at
// path when none exists. It also keeps Hop's private directory out of snapshots.
func EnsureRepository(path string) (*Repository, error) {
	return NewGitStore().Ensure(context.Background(), path)
}

// OpenRepository opens the repository containing path and keeps all Hop state,
// including prompt exports, private to the local machine.
func OpenRepository(path string) (*Repository, error) {
	return NewGitStore().Open(context.Background(), path)
}

// FindRepositoryRoot returns the top-level worktree containing path.
func FindRepositoryRoot(path string) (string, error) {
	return NewGitStore().FindRoot(context.Background(), path)
}

// Ensure opens the repository containing path, or initializes a new repository
// at path if it is not already inside one.
func (s *GitStore) Ensure(ctx context.Context, path string) (*Repository, error) {
	path, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
		if root, findErr := s.FindRoot(ctx, filepath.Dir(path)); findErr == nil {
			return s.openRootForEnsure(ctx, root)
		}
		return nil, fmt.Errorf("cannot initialize a repository at non-directory %q", path)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect repository path: %w", statErr)
	}

	if root, findErr := s.FindRoot(ctx, path); findErr == nil {
		return s.openRootForEnsure(ctx, root)
	}

	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create repository directory: %w", err)
	}
	lockPath, err := repositoryInitLockPath(path)
	if err != nil {
		return nil, err
	}
	release, err := acquireFileLock(ctx, lockPath, "Hop repository initialization")
	if err != nil {
		return nil, err
	}
	defer release()

	// A concurrent Hop process may have initialized this directory while this
	// process waited for the per-user bootstrap lock.
	if root, findErr := s.FindRoot(ctx, path); findErr == nil {
		return s.openRoot(ctx, root, true)
	}
	if _, err := s.run(ctx, path, nil, nil, "init", "--quiet", path); err != nil {
		return nil, err
	}
	return s.openRoot(ctx, path, true)
}

func (s *GitStore) openRootForEnsure(ctx context.Context, root string) (*Repository, error) {
	lockPath, err := repositoryInitLockPath(root)
	if err != nil {
		return nil, err
	}
	release, err := acquireFileLock(ctx, lockPath, "Hop repository initialization")
	if err != nil {
		return nil, err
	}
	defer release()
	return s.openRoot(ctx, root, true)
}

// Open opens the non-bare repository containing path.
func (s *GitStore) Open(ctx context.Context, path string) (*Repository, error) {
	root, err := s.FindRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	return s.openRoot(ctx, root, true)
}

// FindRoot locates the top-level non-bare worktree containing path.
func (s *GitStore) FindRoot(ctx context.Context, path string) (string, error) {
	path, err := existingDirectory(path)
	if err != nil {
		return "", err
	}
	output, err := s.run(ctx, path, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("find Git repository from %q: %w", path, err)
	}
	root := trimLine(output)
	if root == "" {
		return "", fmt.Errorf("git returned an empty repository root for %q", path)
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(path, root)
	}
	return filepath.Clean(root), nil
}

func (s *GitStore) openRoot(ctx context.Context, root string, addExclude bool) (*Repository, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	inside, err := s.run(ctx, root, nil, nil, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return nil, err
	}
	if trimLine(inside) != "true" {
		return nil, fmt.Errorf("%q is not a non-bare Git worktree", root)
	}

	gitDirOutput, err := s.run(ctx, root, nil, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	commonOutput, err := s.run(ctx, root, nil, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	gitDir := resolveGitPath(root, trimLine(gitDirOutput))
	commonGitDir := resolveGitPath(root, trimLine(commonOutput))
	repository := &Repository{
		root:         filepath.Clean(root),
		gitDir:       gitDir,
		commonGitDir: commonGitDir,
		store:        s,
	}
	if addExclude {
		if err := repository.EnsureHopExcluded(); err != nil {
			return nil, err
		}
	}
	return repository, nil
}

// Root returns the absolute top-level directory of this worktree.
func (r *Repository) Root() string { return r.root }

// GitDir returns the absolute per-worktree Git directory.
func (r *Repository) GitDir() string { return r.gitDir }

// CommonGitDir returns the absolute common Git directory shared by linked
// worktrees.
func (r *Repository) CommonGitDir() string { return r.commonGitDir }

// EnsureHopExcluded keeps the entire .hop directory private. Prompt exports can
// contain user requests and machine paths, so they must never enter snapshots.
func (r *Repository) EnsureHopExcluded() error {
	infoDir := filepath.Join(r.commonGitDir, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return fmt.Errorf("create Git info directory: %w", err)
	}
	excludePath := filepath.Join(infoDir, "exclude")
	contents, err := os.ReadFile(excludePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read Git exclude file: %w", err)
	}
	lines := make([]string, 0)
	for _, line := range strings.Split(string(contents), "\n") {
		normalized := strings.TrimSuffix(line, "\r")
		switch normalized {
		case ".hop/", ".hop/*", "!.hop/records/", "!.hop/records/**",
			"# Hop local runtime; .hop/records is intentionally versioned.",
			"# Hop private local runtime and prompt records.":
			continue
		}
		lines = append(lines, line)
	}
	updated := strings.Join(lines, "\n")
	if updated != "" && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	updated += "# Hop private local runtime and prompt records.\n.hop/\n"
	if updated == string(contents) {
		return nil
	}
	if err := os.WriteFile(excludePath, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write Git exclude file: %w", err)
	}
	return nil
}

// Head returns the current HEAD commit. exists is false for an unborn branch.
func (r *Repository) Head(ctx context.Context) (oid string, exists bool, err error) {
	output, err := r.run(ctx, nil, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		if gitExitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return trimLine(output), true, nil
}

// PushAccepted publishes one accepted Hop commit to the repository's existing
// branch destination. It never force-pushes and returns configured=false when
// the repository has no unambiguous remote branch target.
func (r *Repository) PushAccepted(ctx context.Context, commit string) (result RemotePushResult, configured bool, err error) {
	if err := validObjectName(commit); err != nil {
		return result, false, fmt.Errorf("invalid accepted commit: %w", err)
	}
	destination, configured, err := r.pushDestination(ctx)
	if err != nil || !configured {
		return result, configured, err
	}
	if _, err := r.run(ctx, []string{"GIT_TERMINAL_PROMPT=0"}, nil,
		"push", "--porcelain", destination.remote, commit+":"+destination.ref); err != nil {
		return result, true, fmt.Errorf("push accepted commit to %s/%s: %w", destination.remote, strings.TrimPrefix(destination.ref, "refs/heads/"), err)
	}
	return RemotePushResult{Remote: destination.safeRemote, Ref: destination.ref, Commit: commit}, true, nil
}

type pushDestination struct {
	remote     string
	safeRemote string
	ref        string
}

func (r *Repository) pushDestination(ctx context.Context) (pushDestination, bool, error) {
	var result pushDestination
	branch, exists, err := r.optionalGitOutput(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return result, false, fmt.Errorf("discover automatic push branch: %w", err)
	}
	if !exists || branch == "" {
		return result, false, nil
	}
	if _, err := r.run(ctx, nil, nil, "check-ref-format", "--branch", branch); err != nil {
		return result, false, fmt.Errorf("validate automatic push branch: %w", err)
	}

	remoteNamesOutput, err := r.run(ctx, nil, nil, "remote")
	if err != nil {
		return result, false, fmt.Errorf("list Git remotes: %w", err)
	}
	remoteNames := nonemptyLines(remoteNamesOutput)
	if len(remoteNames) == 0 {
		return result, false, nil
	}

	upstreamRemote, hasUpstreamRemote, err := r.optionalGitOutput(ctx, "config", "--get", "branch."+branch+".remote")
	if err != nil {
		return result, false, fmt.Errorf("read Git upstream remote for %s: %w", branch, err)
	}
	remote := ""
	for _, key := range []string{
		"branch." + branch + ".pushRemote",
		"remote.pushDefault",
	} {
		value, found, configErr := r.optionalGitOutput(ctx, "config", "--get", key)
		if configErr != nil {
			return result, false, fmt.Errorf("read Git config %s: %w", key, configErr)
		}
		if found && value != "" {
			remote = value
			break
		}
	}
	if remote == "" && hasUpstreamRemote {
		remote = upstreamRemote
	}
	if remote == "." {
		return result, false, nil
	}
	if remote == "" {
		if containsString(remoteNames, "origin") {
			remote = "origin"
		} else if len(remoteNames) == 1 {
			remote = remoteNames[0]
		} else {
			return result, false, nil
		}
	}
	if !containsString(remoteNames, remote) {
		return result, false, fmt.Errorf("configured automatic push remote %q does not exist", remote)
	}

	ref := "refs/heads/" + branch
	if mergeRef, found, configErr := r.optionalGitOutput(ctx, "config", "--get", "branch."+branch+".merge"); configErr != nil {
		return result, false, fmt.Errorf("read upstream branch for %s: %w", branch, configErr)
	} else if found && remote == upstreamRemote && strings.HasPrefix(mergeRef, "refs/heads/") {
		ref = mergeRef
	}
	if err := r.validateRef(ctx, ref); err != nil {
		return result, false, fmt.Errorf("validate automatic push destination: %w", err)
	}
	safeRemote, _ := RedactPromptSecrets(remote)
	return pushDestination{remote: remote, safeRemote: safeRemote, ref: ref}, true, nil
}

// FetchPushTip fetches the configured destination branch without changing the
// user's branch, index, or working tree. exists is false for a new remote branch.
func (r *Repository) FetchPushTip(ctx context.Context) (tip string, configured, exists bool, err error) {
	destination, configured, err := r.pushDestination(ctx)
	if err != nil || !configured {
		return "", configured, false, err
	}
	output, err := r.run(ctx, []string{"GIT_TERMINAL_PROMPT=0"}, nil,
		"ls-remote", "--refs", destination.remote, destination.ref)
	if err != nil {
		return "", true, false, fmt.Errorf("inspect remote branch %s/%s: %w", destination.remote, strings.TrimPrefix(destination.ref, "refs/heads/"), err)
	}
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return "", true, false, nil
	}
	if len(fields) != 2 || fields[1] != destination.ref {
		return "", true, false, fmt.Errorf("inspect remote branch %s/%s: unexpected ls-remote response", destination.remote, strings.TrimPrefix(destination.ref, "refs/heads/"))
	}
	if err := validObjectName(fields[0]); err != nil {
		return "", true, false, fmt.Errorf("invalid remote branch tip: %w", err)
	}
	if _, err := r.run(ctx, []string{"GIT_TERMINAL_PROMPT=0"}, nil,
		"fetch", "--no-tags", destination.remote, destination.ref); err != nil {
		return "", true, false, fmt.Errorf("fetch remote branch %s/%s: %w", destination.remote, strings.TrimPrefix(destination.ref, "refs/heads/"), err)
	}
	fetched, err := r.run(ctx, nil, nil, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", true, false, fmt.Errorf("resolve fetched remote branch: %w", err)
	}
	fetched = trimLine(fetched)
	if fetched != fields[0] {
		return "", true, false, errors.New("fetched remote branch changed while reconciling; retry")
	}
	return fetched, true, true, nil
}

// MergeBase returns the best common ancestor of two commits.
func (r *Repository) MergeBase(ctx context.Context, left, right string) (string, error) {
	if err := validObjectName(left); err != nil {
		return "", fmt.Errorf("invalid left commit: %w", err)
	}
	if err := validObjectName(right); err != nil {
		return "", fmt.Errorf("invalid right commit: %w", err)
	}
	output, err := r.run(ctx, nil, nil, "merge-base", left, right)
	if err != nil {
		return "", fmt.Errorf("find merge base: %w", err)
	}
	return trimLine(output), nil
}

func (r *Repository) optionalGitOutput(ctx context.Context, args ...string) (string, bool, error) {
	output, err := r.run(ctx, nil, nil, args...)
	if err != nil {
		if gitExitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return trimLine(output), true, nil
}

func nonemptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(strings.TrimSuffix(line, "\r")); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// Snapshot records all tracked files and all non-ignored untracked files in the
// worktree. It uses a disposable index, preserving both the contents and staging
// state of the user's real index. The synthetic commit is parented to HEAD when
// HEAD exists; an unborn repository produces a parentless commit.
func (r *Repository) Snapshot(ctx context.Context, message string) (commitOID, treeOID string, err error) {
	return r.SnapshotWithOptions(ctx, SnapshotOptions{Message: message})
}

// SnapshotWithOptions is Snapshot with explicit synthetic commit controls.
func (r *Repository) SnapshotWithOptions(ctx context.Context, options SnapshotOptions) (commitOID, treeOID string, err error) {
	indexPath, cleanup, err := r.temporaryIndex(true)
	if err != nil {
		return "", "", err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + indexPath, "GIT_OPTIONAL_LOCKS=0"}
	if _, err := r.run(ctx, env, nil, "add", "-A", "--", "."); err != nil {
		return "", "", fmt.Errorf("snapshot workspace: %w", err)
	}
	// The disposable index is copied from the worktree index and therefore
	// carries its stat cache. An agent can rewrite a file to the same size within
	// the filesystem timestamp granularity, making a plain `git add -A`
	// intermittently treat fresh content as unchanged. Renormalizing forces Git
	// to hash every tracked path again while retaining additions/deletions staged
	// by the first pass.
	if _, err := r.run(ctx, env, nil, "add", "--renormalize", "--", "."); err != nil {
		return "", "", fmt.Errorf("rehash workspace snapshot: %w", err)
	}
	treeOutput, err := r.run(ctx, env, nil, "write-tree")
	if err != nil {
		return "", "", fmt.Errorf("write snapshot tree: %w", err)
	}
	treeOID = trimLine(treeOutput)

	parent, hasParent, err := r.Head(ctx)
	if err != nil {
		return "", "", err
	}
	commitOptions := options.Commit
	commitOptions.Message = options.Message
	if commitOptions.Message == "" {
		commitOptions.Message = "hop workspace snapshot"
	}
	if hasParent {
		commitOptions.Parents = []string{parent}
	} else {
		commitOptions.Parents = nil
	}
	commitOID, err = r.CommitTreeWithOptions(ctx, treeOID, commitOptions)
	if err != nil {
		return "", "", err
	}
	return commitOID, treeOID, nil
}

// CommitTree creates a synthetic commit from tree. Parent order is preserved.
func (r *Repository) CommitTree(ctx context.Context, tree string, parents []string, message string) (string, error) {
	return r.CommitTreeWithOptions(ctx, tree, CommitOptions{
		Message: message,
		Parents: append([]string(nil), parents...),
	})
}

// ConfiguredUserIdentity returns the Git identity configured for the user in
// this repository. Both user.name and user.email must be present.
func (r *Repository) ConfiguredUserIdentity(ctx context.Context) (GitIdentity, bool, error) {
	name, nameSet, err := r.configValue(ctx, "user.name")
	if err != nil {
		return GitIdentity{}, false, err
	}
	email, emailSet, err := r.configValue(ctx, "user.email")
	if err != nil {
		return GitIdentity{}, false, err
	}
	if !nameSet || !emailSet {
		return GitIdentity{}, false, nil
	}
	identity := GitIdentity{Name: name, Email: email}
	if err := validateIdentity(identity); err != nil {
		return GitIdentity{}, false, fmt.Errorf("invalid configured user identity: %w", err)
	}
	return identity, true, nil
}

// SyntheticIdentity is Hop's controlled committer identity.
func (r *Repository) SyntheticIdentity() GitIdentity {
	return r.store.identity(GitIdentity{})
}

// CommitTreeWithOptions creates a synthetic commit without invoking hooks or
// consulting the user's Git identity. It does not update any ref.
func (r *Repository) CommitTreeWithOptions(ctx context.Context, tree string, options CommitOptions) (string, error) {
	if err := validObjectName(tree); err != nil {
		return "", fmt.Errorf("invalid tree: %w", err)
	}
	for _, parent := range options.Parents {
		if err := validObjectName(parent); err != nil {
			return "", fmt.Errorf("invalid parent: %w", err)
		}
	}

	author := r.store.identity(options.Author)
	committer := r.store.identity(options.Committer)
	if options.Committer.Name == "" && options.Committer.Email == "" {
		committer = author
	}
	if err := validateIdentity(author); err != nil {
		return "", fmt.Errorf("invalid author identity: %w", err)
	}
	if err := validateIdentity(committer); err != nil {
		return "", fmt.Errorf("invalid committer identity: %w", err)
	}

	authorTime := options.AuthorTime
	if authorTime.IsZero() {
		authorTime = r.store.now()
	}
	committerTime := options.CommitterTime
	if committerTime.IsZero() {
		committerTime = authorTime
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + author.Name,
		"GIT_AUTHOR_EMAIL=" + author.Email,
		"GIT_AUTHOR_DATE=" + authorTime.Format(time.RFC3339),
		"GIT_COMMITTER_NAME=" + committer.Name,
		"GIT_COMMITTER_EMAIL=" + committer.Email,
		"GIT_COMMITTER_DATE=" + committerTime.Format(time.RFC3339),
	}

	args := []string{"-c", "commit.gpgSign=false", "-c", "i18n.commitEncoding=UTF-8", "commit-tree", tree}
	for _, parent := range options.Parents {
		args = append(args, "-p", parent)
	}
	message := options.Message
	if message == "" {
		message = "hop synthetic commit"
	}
	output, err := r.run(ctx, env, []byte(message), args...)
	if err != nil {
		return "", fmt.Errorf("create synthetic commit: %w", err)
	}
	return trimLine(output), nil
}

// HiddenRef returns the full private ref name for name.
func HiddenRef(name string) (string, error) {
	if strings.HasPrefix(name, hiddenRefPrefix) {
		if err := checkRefName(name); err != nil {
			return "", err
		}
		return name, nil
	}
	if strings.HasPrefix(name, "refs/") {
		return "", fmt.Errorf("hidden ref must be below %s", hiddenRefPrefix)
	}
	name = strings.TrimPrefix(name, "/")
	ref := hiddenRefPrefix + name
	if err := checkRefName(ref); err != nil {
		return "", err
	}
	return ref, nil
}

// ReadHiddenRef reads refs/hop/name. exists is false when it has not been
// created yet.
func (r *Repository) ReadHiddenRef(ctx context.Context, name string) (oid string, exists bool, err error) {
	ref, err := HiddenRef(name)
	if err != nil {
		return "", false, err
	}
	return r.ReadRef(ctx, ref)
}

// ListHiddenRefs reads all refs below refs/hop in one Git process. Keys are
// relative to refs/hop/ (for example, "states/P_..." or "accepted").
func (r *Repository) ListHiddenRefs(ctx context.Context) (map[string]string, error) {
	output, err := r.run(ctx, nil, nil, "for-each-ref", "--format=%(refname) %(objectname)", hiddenRefPrefix)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		name, oid, found := strings.Cut(line, " ")
		if !found || !strings.HasPrefix(name, hiddenRefPrefix) {
			return nil, fmt.Errorf("unexpected hidden ref listing %q", line)
		}
		refs[strings.TrimPrefix(name, hiddenRefPrefix)] = strings.TrimSpace(oid)
	}
	return refs, nil
}

// UpdateHiddenRef atomically updates refs/hop/name. With no expectedOld value,
// it is unconditional. Passing expectedOld performs compare-and-swap; an empty
// expected value means the ref must not exist.
func (r *Repository) UpdateHiddenRef(ctx context.Context, name, newOID string, expectedOld ...string) error {
	ref, err := HiddenRef(name)
	if err != nil {
		return err
	}
	return r.UpdateRef(ctx, ref, newOID, expectedOld...)
}

// ReadRef reads an exact, fully qualified ref without resolving ambiguous short
// names. exists is false when the ref is absent.
func (r *Repository) ReadRef(ctx context.Context, ref string) (oid string, exists bool, err error) {
	if err := r.validateRef(ctx, ref); err != nil {
		return "", false, err
	}
	output, err := r.run(ctx, nil, nil, "show-ref", "--verify", "--hash", ref)
	if err != nil {
		if gitExitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return trimLine(output), true, nil
}

// UpdateRef updates a fully qualified ref. Supplying expectedOld makes the
// operation compare-and-swap; expectedOld == "" requires an absent ref.
func (r *Repository) UpdateRef(ctx context.Context, ref, newOID string, expectedOld ...string) error {
	if err := r.validateRef(ctx, ref); err != nil {
		return err
	}
	if err := validObjectName(newOID); err != nil {
		return fmt.Errorf("invalid new object ID: %w", err)
	}
	if len(expectedOld) > 1 {
		return fmt.Errorf("update ref accepts at most one expected old object ID")
	}
	args := []string{"update-ref", "--create-reflog", "-m", "hop update", ref, newOID}
	if len(expectedOld) == 1 {
		old := expectedOld[0]
		if old == "" {
			var err error
			old, err = r.ZeroOID(ctx)
			if err != nil {
				return err
			}
		} else if err := validObjectName(old); err != nil {
			return fmt.Errorf("invalid expected object ID: %w", err)
		}
		args = append(args, old)
	}
	if _, err := r.run(ctx, nil, nil, args...); err != nil {
		return fmt.Errorf("update ref %s: %w", ref, err)
	}
	return nil
}

// DeleteRef deletes a fully qualified ref, optionally only if it still has the
// expected object ID.
func (r *Repository) DeleteRef(ctx context.Context, ref string, expectedOld ...string) error {
	if err := r.validateRef(ctx, ref); err != nil {
		return err
	}
	if len(expectedOld) > 1 {
		return fmt.Errorf("delete ref accepts at most one expected old object ID")
	}
	args := []string{"update-ref", "-d", ref}
	if len(expectedOld) == 1 && expectedOld[0] != "" {
		if err := validObjectName(expectedOld[0]); err != nil {
			return fmt.Errorf("invalid expected object ID: %w", err)
		}
		args = append(args, expectedOld[0])
	}
	if _, err := r.run(ctx, nil, nil, args...); err != nil {
		return fmt.Errorf("delete ref %s: %w", ref, err)
	}
	return nil
}

// AddDetachedWorktree materializes commit at path without creating or moving a
// branch. The returned repository is rooted at the new linked worktree.
func (r *Repository) AddDetachedWorktree(ctx context.Context, path, commit string) (*Repository, error) {
	if err := validObjectName(commit); err != nil {
		return nil, fmt.Errorf("invalid worktree commit: %w", err)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree path: %w", err)
	}
	if _, err := r.run(ctx, nil, nil, "worktree", "add", "--detach", path, commit); err != nil {
		return nil, fmt.Errorf("create detached worktree: %w", err)
	}
	worktree, err := r.store.openRoot(ctx, path, true)
	if err != nil {
		return nil, err
	}
	return worktree, nil
}

// RemoveWorktree removes a linked worktree. force allows removal when it has
// local modifications or untracked files.
func (r *Repository) RemoveWorktree(ctx context.Context, path string, force bool) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve worktree path: %w", err)
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	if _, err := r.run(ctx, nil, nil, args...); err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	return nil
}

// Diff returns a binary-capable, full-index patch between two commits or trees.
// An empty endpoint denotes Git's empty tree, which supports unborn histories.
func (r *Repository) Diff(ctx context.Context, from, to string) (string, error) {
	fromTree, err := r.resolveTree(ctx, from)
	if err != nil {
		return "", err
	}
	toTree, err := r.resolveTree(ctx, to)
	if err != nil {
		return "", err
	}
	output, err := r.run(ctx, nil, nil,
		"diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", "--find-renames",
		fromTree, toTree, "--",
	)
	if err != nil {
		return "", fmt.Errorf("diff trees: %w", err)
	}
	return output, nil
}

// ChangedPathDetails returns Git's rename-aware path changes.
func (r *Repository) ChangedPathDetails(ctx context.Context, from, to string) ([]PathChange, error) {
	fromTree, err := r.resolveTree(ctx, from)
	if err != nil {
		return nil, err
	}
	toTree, err := r.resolveTree(ctx, to)
	if err != nil {
		return nil, err
	}
	output, err := r.run(ctx, nil, nil,
		"diff", "--no-ext-diff", "--no-textconv", "--name-status", "-z", "--find-renames",
		fromTree, toTree, "--",
	)
	if err != nil {
		return nil, fmt.Errorf("list changed paths: %w", err)
	}
	return parseNameStatus([]byte(output))
}

// ChangedPaths returns the set of affected paths in bytewise order. A rename
// or copy contributes both its old and new path, making overlap checks safe.
func (r *Repository) ChangedPaths(ctx context.Context, from, to string) ([]string, error) {
	changes, err := r.ChangedPathDetails(ctx, from, to)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		if change.OldPath != "" {
			paths[change.OldPath] = struct{}{}
		}
		if change.NewPath != "" {
			paths[change.NewPath] = struct{}{}
		}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

// WorktreeTree snapshots the visible worktree relative to an expected Hop
// tree. The caller's real Git index is never read or changed. Seeding from the
// expected tree makes Hop-projected files remain visible to the snapshot even
// when they are untracked by the user's branch or matched by an ignore rule.
func (r *Repository) WorktreeTree(ctx context.Context, expected string) (string, error) {
	expectedTree, err := r.resolveTree(ctx, expected)
	if err != nil {
		return "", err
	}
	indexPath, cleanup, err := r.temporaryIndex(false)
	if err != nil {
		return "", err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + indexPath, "GIT_OPTIONAL_LOCKS=0"}
	if _, err := r.run(ctx, env, nil, "read-tree", expectedTree); err != nil {
		return "", fmt.Errorf("seed visible-root snapshot: %w", err)
	}
	if _, err := r.run(ctx, env, nil, "add", "-A", "--", "."); err != nil {
		return "", fmt.Errorf("snapshot visible project root: %w", err)
	}
	output, err := r.run(ctx, env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write visible-root tree: %w", err)
	}
	return trimLine(output), nil
}

// CheckIndexSafe verifies that the user's real index contains no staged state
// outside either their current HEAD or the Hop tree already visible in the
// project root. Hop never writes this index, but refusing divergent staging
// prevents a landing from obscuring the user's in-progress Git operation.
func (r *Repository) CheckIndexSafe(ctx context.Context, visibleTree string) error {
	visibleTree, err := r.resolveTree(ctx, visibleTree)
	if err != nil {
		return err
	}
	head, exists, err := r.Head(ctx)
	if err != nil {
		return err
	}
	headTree := ""
	if exists {
		headTree, err = r.resolveTree(ctx, head)
	} else {
		headTree, err = r.EmptyTree(ctx)
	}
	if err != nil {
		return err
	}
	indexTree, err := r.userIndexTree(ctx)
	if err != nil {
		return &RootConflictError{Reason: "visible project root has unmerged or intent-to-add entries in the real Git index"}
	}
	if indexTree == headTree || indexTree == visibleTree {
		return nil
	}
	paths, err := r.ChangedPaths(ctx, headTree, indexTree)
	if err != nil {
		return err
	}
	return &RootConflictError{
		Paths:  paths,
		Reason: "visible project root has staged Git changes that do not match HEAD or its materialized Hop state",
	}
}

func (r *Repository) userIndexTree(ctx context.Context) (string, error) {
	indexPath := filepath.Join(r.gitDir, "index")
	if _, err := os.Stat(indexPath); errors.Is(err, os.ErrNotExist) {
		return r.EmptyTree(ctx)
	} else if err != nil {
		return "", fmt.Errorf("inspect real Git index: %w", err)
	}
	output, err := r.run(ctx, []string{
		"GIT_INDEX_FILE=" + indexPath,
		"GIT_OPTIONAL_LOCKS=0",
	}, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("read real Git index tree: %w", err)
	}
	return trimLine(output), nil
}

// MaterializationConflicts finds filesystem entries that a tree projection
// would overwrite even though they are absent from the source tree. This
// catches ignored files, which intentionally do not appear in WorktreeTree.
func (r *Repository) MaterializationConflicts(ctx context.Context, from, to string) ([]string, error) {
	fromPaths, err := r.treeLeafPaths(ctx, from)
	if err != nil {
		return nil, err
	}
	toPaths, err := r.treeLeafPaths(ctx, to)
	if err != nil {
		return nil, err
	}
	conflicts := map[string]struct{}{}
	for path := range toPaths {
		if _, existed := fromPaths[path]; existed {
			continue
		}
		parts := strings.Split(path, "/")
		for index := range parts {
			prefix := strings.Join(parts[:index+1], "/")
			info, statErr := os.Lstat(filepath.Join(r.root, filepath.FromSlash(prefix)))
			if statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) || errors.Is(statErr, syscall.ENOTDIR) {
					break
				}
				return nil, fmt.Errorf("inspect visible path %s: %w", prefix, statErr)
			}
			if index < len(parts)-1 {
				if info.IsDir() {
					continue
				}
				if _, expected := fromPaths[prefix]; expected {
					break
				}
				conflicts[prefix] = struct{}{}
				break
			}
			if info.IsDir() {
				unexpected, walkErr := r.unexpectedDirectoryLeaves(path, fromPaths)
				if walkErr != nil {
					return nil, walkErr
				}
				for _, unexpectedPath := range unexpected {
					conflicts[unexpectedPath] = struct{}{}
				}
				continue
			}
			conflicts[path] = struct{}{}
		}
	}
	paths := make([]string, 0, len(conflicts))
	for path := range conflicts {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func (r *Repository) unexpectedDirectoryLeaves(path string, expected map[string]struct{}) ([]string, error) {
	root := filepath.Join(r.root, filepath.FromSlash(path))
	var conflicts []string
	err := filepath.WalkDir(root, func(candidate string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if candidate == root || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(r.root, candidate)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if _, ok := expected[relative]; !ok {
			conflicts = append(conflicts, relative)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect visible directory %s: %w", path, err)
	}
	sort.Strings(conflicts)
	return conflicts, nil
}

// MaterializeTree updates only the visible worktree from one accepted tree to
// another. HEAD, the current branch, and the user's real index remain exactly
// where they were. The operation fails closed when the worktree no longer
// matches from or an ignored destination would be overwritten.
func (r *Repository) MaterializeTree(ctx context.Context, from, to string) error {
	fromTree, err := r.resolveTree(ctx, from)
	if err != nil {
		return err
	}
	toTree, err := r.resolveTree(ctx, to)
	if err != nil {
		return err
	}
	if err := r.CheckIndexSafe(ctx, fromTree); err != nil {
		return err
	}
	actualTree, err := r.WorktreeTree(ctx, fromTree)
	if err != nil {
		return err
	}
	if actualTree != fromTree {
		paths, pathErr := r.ChangedPaths(ctx, fromTree, actualTree)
		if pathErr != nil {
			return pathErr
		}
		return &RootConflictError{Paths: paths}
	}
	conflicts, err := r.MaterializationConflicts(ctx, fromTree, toTree)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return &RootConflictError{
			Paths:  conflicts,
			Reason: "visible project root contains ignored or untracked paths that landing would overwrite",
		}
	}
	if fromTree == toTree {
		return nil
	}

	indexPath, cleanup, err := r.temporaryIndex(false)
	if err != nil {
		return err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + indexPath, "GIT_OPTIONAL_LOCKS=0"}
	if _, err := r.run(ctx, env, nil, "read-tree", fromTree); err != nil {
		return fmt.Errorf("seed visible-root materialization: %w", err)
	}
	if _, err := r.run(ctx, env, nil, "update-index", "--refresh"); err != nil {
		return &RootConflictError{Reason: "visible project root changed while Hop was preparing to synchronize it"}
	}
	if _, err := r.run(ctx, env, nil, "read-tree", "-m", "-u", fromTree, toTree); err != nil {
		return fmt.Errorf("materialize accepted tree into visible project root: %w", err)
	}
	materializedTree, err := r.WorktreeTree(ctx, toTree)
	if err != nil {
		return err
	}
	if materializedTree != toTree {
		paths, pathErr := r.ChangedPaths(ctx, toTree, materializedTree)
		if pathErr != nil {
			return pathErr
		}
		return &RootConflictError{
			Paths:  paths,
			Reason: "visible project root changed while Hop was synchronizing it",
		}
	}
	return nil
}

func (r *Repository) treeLeafPaths(ctx context.Context, object string) (map[string]struct{}, error) {
	tree, err := r.resolveTree(ctx, object)
	if err != nil {
		return nil, err
	}
	output, err := r.run(ctx, nil, nil, "ls-tree", "-r", "-z", "--name-only", tree)
	if err != nil {
		return nil, fmt.Errorf("list tree paths: %w", err)
	}
	paths := make(map[string]struct{})
	for _, path := range splitNull([]byte(output)) {
		if path != "" {
			paths[path] = struct{}{}
		}
	}
	return paths, nil
}

// TrackedPaths lists index entries at or below path. Hop uses this before
// initialization to avoid hiding or overwriting a repository that already
// treats .hop as user-owned source content.
func (r *Repository) TrackedPaths(ctx context.Context, path string) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("tracked path query requires a path")
	}
	output, err := r.run(ctx, nil, nil, "ls-files", "-z", "--", path)
	if err != nil {
		return nil, fmt.Errorf("list tracked paths below %s: %w", path, err)
	}
	parts := splitNull([]byte(output))
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			paths = append(paths, part)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// ComposeTrees performs Git's real three-way merge without touching a
// worktree or index. Independent hunks in the same file, identical changes,
// renames, and compatible mode/content changes merge automatically. On a
// genuine conflict, tree is the best-effort conflict-marker tree and conflicts
// lists the paths an agent must reconcile.
func (r *Repository) ComposeTrees(ctx context.Context, base, ours, theirs string) (tree string, conflicts []string, err error) {
	baseCommit, err := r.resolveCommit(ctx, base)
	if err != nil {
		return "", nil, fmt.Errorf("resolve base commit: %w", err)
	}
	oursCommit, err := r.resolveCommit(ctx, ours)
	if err != nil {
		return "", nil, fmt.Errorf("resolve current commit: %w", err)
	}
	theirsCommit, err := r.resolveCommit(ctx, theirs)
	if err != nil {
		return "", nil, fmt.Errorf("resolve proposal commit: %w", err)
	}

	output, mergeErr := r.run(ctx, []string{"GIT_OPTIONAL_LOCKS=0"}, nil,
		"-c", "merge.conflictStyle=zdiff3",
		"-c", "merge.renames=true",
		"-c", "merge.directoryRenames=conflict",
		"merge-tree", "--write-tree", "--merge-base", baseCommit,
		"--name-only", "-z", "--no-messages", oursCommit, theirsCommit,
	)
	fields := splitNull([]byte(output))
	if len(fields) == 0 || fields[0] == "" {
		if mergeErr != nil {
			return "", nil, fmt.Errorf("compose trees: %w", mergeErr)
		}
		return "", nil, errors.New("git merge-tree returned no tree")
	}
	tree = fields[0]
	if err := validObjectName(tree); err != nil {
		return "", nil, fmt.Errorf("invalid composed tree: %w", err)
	}
	conflictSet := map[string]struct{}{}
	for _, path := range fields[1:] {
		if path != "" {
			conflictSet[path] = struct{}{}
		}
	}
	for path := range conflictSet {
		conflicts = append(conflicts, path)
	}
	sort.Strings(conflicts)
	if mergeErr == nil {
		if len(conflicts) > 0 {
			return "", nil, errors.New("git merge-tree reported conflict paths with a successful exit")
		}
		return tree, nil, nil
	}
	if gitExitCode(mergeErr) != 1 {
		return "", nil, fmt.Errorf("compose trees: %w", mergeErr)
	}
	if len(conflicts) == 0 {
		// Git documents conflict classes (notably some directory-renames) that
		// produce no conflicted-file list. Give the agent the changed-path union
		// as useful reconciliation candidates instead of converting a real merge
		// conflict into an internal error.
		for _, side := range []string{oursCommit, theirsCommit} {
			paths, pathsErr := r.ChangedPaths(ctx, baseCommit, side)
			if pathsErr != nil {
				return "", nil, fmt.Errorf("derive structural conflict paths: %w", pathsErr)
			}
			for _, path := range paths {
				conflictSet[path] = struct{}{}
			}
		}
		for path := range conflictSet {
			conflicts = append(conflicts, path)
		}
		sort.Strings(conflicts)
		if len(conflicts) == 0 {
			conflicts = []string{"(structural merge conflict; inspect both inputs)"}
		}
	}
	return tree, conflicts, nil
}

// VerifyObject verifies that name resolves to an object in this repository.
func (r *Repository) VerifyObject(ctx context.Context, name string) error {
	if err := validObjectName(name); err != nil {
		return err
	}
	if _, err := r.run(ctx, nil, nil, "cat-file", "-e", name+"^{object}"); err != nil {
		return fmt.Errorf("verify Git object %s: %w", name, err)
	}
	return nil
}

// VerifyObjects verifies each named object.
func (r *Repository) VerifyObjects(ctx context.Context, names ...string) error {
	for _, name := range names {
		if err := r.VerifyObject(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// Verify checks the connectivity and validity of the repository's reachable
// object graph. Dangling Hop snapshots are allowed and do not produce noise.
func (r *Repository) Verify(ctx context.Context) error {
	if _, err := r.run(ctx, nil, nil, "fsck", "--connectivity-only", "--no-dangling"); err != nil {
		return fmt.Errorf("verify Git object database: %w", err)
	}
	return nil
}

// EmptyTree returns the object ID of Git's empty tree for this repository's
// object format.
func (r *Repository) EmptyTree(ctx context.Context) (string, error) {
	output, err := r.run(ctx, nil, nil, "mktree")
	if err != nil {
		return "", fmt.Errorf("create empty tree: %w", err)
	}
	return trimLine(output), nil
}

// ZeroOID returns the all-zero object ID of the correct length for this
// repository (SHA-1 or SHA-256).
func (r *Repository) ZeroOID(ctx context.Context) (string, error) {
	output, err := r.run(ctx, nil, nil, "rev-parse", "--show-object-format")
	if err != nil {
		return "", fmt.Errorf("read Git object format: %w", err)
	}
	switch trimLine(output) {
	case "sha1":
		return strings.Repeat("0", 40), nil
	case "sha256":
		return strings.Repeat("0", 64), nil
	default:
		return "", fmt.Errorf("unsupported Git object format %q", trimLine(output))
	}
}

func (r *Repository) resolveTree(ctx context.Context, object string) (string, error) {
	if object == "" {
		return r.EmptyTree(ctx)
	}
	if err := validObjectName(object); err != nil {
		return "", err
	}
	output, err := r.run(ctx, nil, nil, "rev-parse", "--verify", "--quiet", object+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("resolve tree %s: %w", object, err)
	}
	return trimLine(output), nil
}

func (r *Repository) resolveCommit(ctx context.Context, object string) (string, error) {
	if err := validObjectName(object); err != nil {
		return "", err
	}
	output, err := r.run(ctx, nil, nil, "rev-parse", "--verify", "--quiet", object+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve commit %s: %w", object, err)
	}
	return trimLine(output), nil
}

func (r *Repository) validateRef(ctx context.Context, ref string) error {
	if !strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("ref must be fully qualified: %q", ref)
	}
	if _, err := r.run(ctx, nil, nil, "check-ref-format", ref); err != nil {
		return fmt.Errorf("invalid ref %q: %w", ref, err)
	}
	return nil
}

func (r *Repository) temporaryIndex(seedFromUser bool) (string, func(), error) {
	if err := os.MkdirAll(r.gitDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("prepare Git directory: %w", err)
	}
	temporary, err := os.CreateTemp(r.gitDir, "hop-index-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary Git index: %w", err)
	}
	path := temporary.Name()
	if err := temporary.Close(); err != nil {
		os.Remove(path)
		return "", nil, fmt.Errorf("close temporary Git index: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(path)
		_ = os.Remove(path + ".lock")
	}

	if seedFromUser {
		userIndex := filepath.Join(r.gitDir, "index")
		contents, readErr := os.ReadFile(userIndex)
		switch {
		case readErr == nil:
			if writeErr := os.WriteFile(path, contents, 0o600); writeErr != nil {
				cleanup()
				return "", nil, fmt.Errorf("seed temporary Git index: %w", writeErr)
			}
		case errors.Is(readErr, os.ErrNotExist):
			if removeErr := os.Remove(path); removeErr != nil {
				cleanup()
				return "", nil, fmt.Errorf("prepare empty temporary Git index: %w", removeErr)
			}
		default:
			cleanup()
			return "", nil, fmt.Errorf("read user Git index: %w", readErr)
		}
	} else if err := os.Remove(path); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("prepare temporary Git index: %w", err)
	}
	return path, cleanup, nil
}

func (r *Repository) run(ctx context.Context, env []string, stdin []byte, args ...string) (string, error) {
	return r.store.run(ctx, r.root, env, stdin, args...)
}

func (s *GitStore) run(ctx context.Context, directory string, env []string, stdin []byte, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	binary := s.Binary
	if binary == "" {
		binary = defaultGitBinary
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = directory
	command.Env = mergeEnvironment(os.Environ(), append([]string{"LC_ALL=C"}, env...))
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return stdout.String(), &GitError{
			Args:     append([]string(nil), args...),
			ExitCode: exitCode,
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			Err:      err,
		}
	}
	return stdout.String(), nil
}

func (s *GitStore) identity(override GitIdentity) GitIdentity {
	identity := s.Identity
	if identity.Name == "" {
		identity.Name = defaultCommitName
	}
	if identity.Email == "" {
		identity.Email = defaultCommitMail
	}
	if override.Name != "" {
		identity.Name = override.Name
	}
	if override.Email != "" {
		identity.Email = override.Email
	}
	return identity
}

func (r *Repository) configValue(ctx context.Context, key string) (string, bool, error) {
	output, err := r.run(ctx, nil, nil, "config", "--get", key)
	if err != nil {
		var gitErr *GitError
		if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read git config %s: %w", key, err)
	}
	value := trimLine(output)
	return value, value != "", nil
}

func (s *GitStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func parseNameStatus(output []byte) ([]PathChange, error) {
	fields := splitNull(output)
	changes := make([]PathChange, 0, len(fields)/2)
	for index := 0; index < len(fields); {
		status := fields[index]
		index++
		if status == "" {
			continue
		}
		kind := status[0]
		if kind == 'R' || kind == 'C' {
			if index+1 >= len(fields) {
				return nil, fmt.Errorf("malformed rename entry in git diff --name-status")
			}
			changes = append(changes, PathChange{
				Status:  status,
				OldPath: fields[index],
				NewPath: fields[index+1],
			})
			index += 2
			continue
		}
		if index >= len(fields) {
			return nil, fmt.Errorf("malformed entry in git diff --name-status")
		}
		changes = append(changes, PathChange{Status: status, NewPath: fields[index]})
		index++
	}
	return changes, nil
}

func parseUnmergedPaths(output []byte) []string {
	set := make(map[string]struct{})
	for _, record := range splitNull(output) {
		if record == "" {
			continue
		}
		if tab := strings.IndexByte(record, '\t'); tab >= 0 && tab+1 < len(record) {
			set[record[tab+1:]] = struct{}{}
		}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func splitNull(value []byte) []string {
	if len(value) == 0 {
		return nil
	}
	parts := bytes.Split(value, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	result := make([]string, len(parts))
	for index, part := range parts {
		result[index] = string(part)
	}
	return result
}

func validObjectName(name string) error {
	if name == "" {
		return fmt.Errorf("object name is empty")
	}
	if strings.HasPrefix(name, "-") || strings.ContainsAny(name, "\x00\r\n") {
		return fmt.Errorf("unsafe object name %q", name)
	}
	return nil
}

func validateIdentity(identity GitIdentity) error {
	if identity.Name == "" || identity.Email == "" {
		return fmt.Errorf("name and email are required")
	}
	if strings.ContainsAny(identity.Name, "\x00\r\n<>") {
		return fmt.Errorf("unsafe name")
	}
	if strings.ContainsAny(identity.Email, "\x00\r\n<>") {
		return fmt.Errorf("unsafe email")
	}
	return nil
}

func checkRefName(ref string) error {
	if ref == "" || strings.ContainsAny(ref, "\x00\r\n") {
		return fmt.Errorf("invalid ref %q", ref)
	}
	if !strings.HasPrefix(ref, "refs/") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "@{") ||
		strings.ContainsAny(ref, " ~^:?*[\\") {
		return fmt.Errorf("invalid ref %q", ref)
	}
	for _, component := range strings.Split(ref, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") {
			return fmt.Errorf("invalid ref %q", ref)
		}
	}
	return nil
}

func gitExitCode(err error) int {
	var gitErr *GitError
	if errors.As(err, &gitErr) {
		return gitErr.ExitCode
	}
	return -1
}

func mergeEnvironment(base, overrides []string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))
	add := func(entry string) {
		key := entry
		if equals := strings.IndexByte(entry, '='); equals >= 0 {
			key = entry[:equals]
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = entry
	}
	for _, entry := range base {
		add(entry)
	}
	for _, entry := range overrides {
		add(entry)
	}
	result := make([]string, 0, len(values))
	for _, key := range order {
		result = append(result, values[key])
	}
	return result
}

func absolutePath(path string) (string, error) {
	if path == "" {
		path = "."
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	return filepath.Clean(absolute), nil
}

func existingDirectory(path string) (string, error) {
	path, err := absolutePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect path %q: %w", path, err)
	}
	if !info.IsDir() {
		path = filepath.Dir(path)
	}
	return path, nil
}

func resolveGitPath(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(root, path))
}

func trimLine(value string) string {
	return strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r")
}
