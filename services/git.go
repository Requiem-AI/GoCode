package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	appctx "github.com/requiem-ai/gocode/context"
)

const GIT_SVC = "git_svc"

type GitRepo struct {
	ChatID        int64
	ThreadID      int
	Name          string
	Path          string
	DefaultBranch string
}

type GitService struct {
	appctx.DefaultService

	BaseDir string

	mu    sync.Mutex
	repos map[string]*GitRepo
}

func (svc GitService) Id() string {
	return GIT_SVC
}

func (svc *GitService) Configure(ctx *appctx.Context) error {
	if err := svc.DefaultService.Configure(ctx); err != nil {
		return err
	}

	baseDir := strings.TrimSpace(os.Getenv("GIT_REPO_ROOT"))
	if baseDir == "" {
		baseDir = "repos"
	}

	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(absBase, 0o755); err != nil {
		return err
	}

	svc.BaseDir = absBase
	svc.repos = make(map[string]*GitRepo)

	return nil
}

func (svc *GitService) TopicRepoPath(chatID int64, threadID int) string {
	return filepath.Join(svc.BaseDir, fmt.Sprintf("%d_%d", chatID, threadID))
}

func (svc *GitService) EnsureTopicRepo(chatID int64, threadID int) (*GitRepo, error) {
	if threadID == 0 {
		return nil, errors.New("missing topic thread id")
	}

	key := topicKey(chatID, threadID)

	svc.mu.Lock()
	repo := svc.repos[key]
	svc.mu.Unlock()

	if repo != nil {
		return repo, nil
	}

	repoPath := svc.TopicRepoPath(chatID, threadID)
	if err := svc.initRepo(repoPath); err != nil {
		return nil, err
	}

	repo = &GitRepo{
		ChatID:        chatID,
		ThreadID:      threadID,
		Path:          repoPath,
		DefaultBranch: "main",
	}

	svc.mu.Lock()
	svc.repos[key] = repo
	svc.mu.Unlock()

	return repo, nil
}

func (svc *GitService) CreateFeatureBranch(repo *GitRepo, feature string) (string, error) {
	if repo == nil {
		return "", errors.New("repo is nil")
	}

	featureSlug := slugify(feature)
	if featureSlug == "" {
		featureSlug = "feature"
	}

	if len(featureSlug) > 40 {
		featureSlug = featureSlug[:40]
	}

	branch := fmt.Sprintf("feature/%s-%s", featureSlug, time.Now().UTC().Format("20060102-150405"))

	if err := svc.checkoutBranch(repo.Path, repo.DefaultBranch); err != nil {
		return "", err
	}

	if err := svc.checkoutBranch(repo.Path, branch); err != nil {
		return "", err
	}

	return branch, nil
}

func (svc *GitService) initRepo(repoPath string) error {
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		return err
	}

	if svc.isGitRepo(repoPath) {
		return nil
	}

	if err := svc.runGit(repoPath, "init", "-b", "main"); err == nil {
		return nil
	}

	if err := svc.runGit(repoPath, "init"); err != nil {
		return err
	}

	return svc.runGit(repoPath, "checkout", "-b", "main")
}

func (svc *GitService) isGitRepo(repoPath string) bool {
	cmd := exec.CommandContext(context.Background(), "git", "-C", repoPath, "rev-parse", "--is-inside-work-tree")
	return cmd.Run() == nil
}

func (svc *GitService) checkoutBranch(repoPath, branch string) error {
	if branch == "" {
		return errors.New("branch is empty")
	}

	if svc.branchExists(repoPath, branch) {
		return svc.runGit(repoPath, "checkout", branch)
	}

	return svc.runGit(repoPath, "checkout", "-b", branch)
}

func (svc *GitService) branchExists(repoPath, branch string) bool {
	cmd := exec.CommandContext(context.Background(), "git", "-C", repoPath, "rev-parse", "--verify", branch)
	return cmd.Run() == nil
}

func (svc *GitService) runGit(repoPath string, args ...string) error {
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", repoPath}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(in string) string {
	in = strings.ToLower(strings.TrimSpace(in))
	if in == "" {
		return ""
	}
	out := slugRe.ReplaceAllString(in, "-")
	out = strings.Trim(out, "-")
	return out
}

func topicKey(chatID int64, threadID int) string {
	return fmt.Sprintf("%d:%d", chatID, threadID)
}
