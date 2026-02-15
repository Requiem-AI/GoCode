package services

import (
	"context"
	"encoding/base64"
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

	if err := os.MkdirAll(absBase, 0o775); err != nil {
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
	return svc.ensureTopicRepo(chatID, threadID, "", "")
}

func (svc *GitService) EnsureTopicRepoFrom(chatID int64, threadID int, repoURL, token string) (*GitRepo, error) {
	return svc.ensureTopicRepo(chatID, threadID, repoURL, token)
}

func (svc *GitService) EnsureTopicRepoFromPath(chatID int64, threadID int, repoPath string) (*GitRepo, error) {
	if threadID == 0 {
		return nil, errors.New("missing topic thread id")
	}

	trimmed := strings.TrimSpace(repoPath)
	if trimmed == "" {
		return nil, errors.New("repo path is empty")
	}

	if strings.HasPrefix(trimmed, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			trimmed = filepath.Join(home, strings.TrimPrefix(trimmed, "~"))
		}
	}

	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("repo path is not a directory")
	}
	if !svc.isGitRepo(absPath) {
		return nil, errors.New("repo path is not a git repository")
	}

	key := topicKey(chatID, threadID)
	svc.mu.Lock()
	repo := svc.repos[key]
	svc.mu.Unlock()

	if repo != nil {
		return repo, nil
	}

	defaultBranch := svc.defaultBranch(absPath)
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	repo = &GitRepo{
		ChatID:        chatID,
		ThreadID:      threadID,
		Path:          absPath,
		DefaultBranch: defaultBranch,
	}

	svc.mu.Lock()
	svc.repos[key] = repo
	svc.mu.Unlock()

	return repo, nil
}

func (svc *GitService) GitHubToken() string {
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}

func (svc *GitService) SetGitHubToken(token string) error {
	if err := os.Setenv("GITHUB_TOKEN", token); err != nil {
		return err
	}

	envPath, err := envFilePath()
	if err != nil {
		return err
	}

	return updateEnvFile(envPath, map[string]string{
		"GITHUB_TOKEN": token,
	})
}

func (svc *GitService) GitHubUseSSH() bool {
	return isEnvTrue(os.Getenv("GITHUB_USE_SSH"))
}

func (svc *GitService) GitHubSSHKeyPath() (string, error) {
	keyPath := strings.TrimSpace(os.Getenv("GITHUB_SSH_KEY_PATH"))
	if keyPath != "" {
		return keyPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "id_ed25519_gocode"), nil
}

func (svc *GitService) EnsureSSHKey(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh-keygen", "-t", "ed25519", "-f", path, "-N", "", "-C", "gocode")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (svc *GitService) CheckGitHubSSH(keyPath string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(
		ctx,
		"ssh",
		"-T",
		"git@github.com",
		"-i",
		keyPath,
		"-o",
		"IdentitiesOnly=yes",
		"-o",
		"BatchMode=yes",
		"-o",
		"StrictHostKeyChecking=accept-new",
	)
	out, err := cmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err == nil {
		return true, msg, nil
	}
	if strings.Contains(msg, "successfully authenticated") {
		return true, msg, nil
	}
	if strings.Contains(strings.ToLower(msg), "permission denied") {
		return false, msg, nil
	}
	return false, msg, err
}

func (svc *GitService) SetGitHubSSHConfig(keyPath string, enabled bool) error {
	if err := os.Setenv("GITHUB_USE_SSH", boolEnv(enabled)); err != nil {
		return err
	}
	if err := os.Setenv("GITHUB_SSH_KEY_PATH", keyPath); err != nil {
		return err
	}

	envPath, err := envFilePath()
	if err != nil {
		return err
	}

	return updateEnvFile(envPath, map[string]string{
		"GITHUB_USE_SSH":      boolEnv(enabled),
		"GITHUB_SSH_KEY_PATH": keyPath,
	})
}

func (svc *GitService) ClearGitHubAuth() error {
	if err := svc.SetGitHubToken(""); err != nil {
		return err
	}
	keyPath, err := svc.GitHubSSHKeyPath()
	if err != nil {
		return err
	}
	return svc.SetGitHubSSHConfig(keyPath, false)
}

func (svc *GitService) DeleteTopicRepo(chatID int64, threadID int) error {
	if threadID == 0 {
		return errors.New("missing topic thread id")
	}

	repoPath := svc.TopicRepoPath(chatID, threadID)
	cleanPath := filepath.Clean(repoPath)
	base := filepath.Clean(svc.BaseDir)
	if !strings.HasPrefix(cleanPath+string(os.PathSeparator), base+string(os.PathSeparator)) {
		return fmt.Errorf("refusing to delete repo outside base dir: %s", cleanPath)
	}

	key := topicKey(chatID, threadID)
	svc.mu.Lock()
	delete(svc.repos, key)
	svc.mu.Unlock()

	return os.RemoveAll(cleanPath)
}

func (svc *GitService) ensureTopicRepo(chatID int64, threadID int, repoURL, token string) (*GitRepo, error) {
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
	if svc.isGitRepo(repoPath) {
		repoURL = ""
	}

	if repoURL != "" {
		if err := svc.cloneRepo(repoURL, repoPath, token); err != nil {
			return nil, err
		}
	} else {
		if err := svc.initRepo(repoPath); err != nil {
			return nil, err
		}
	}

	defaultBranch := svc.defaultBranch(repoPath)
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	repo = &GitRepo{
		ChatID:        chatID,
		ThreadID:      threadID,
		Path:          repoPath,
		DefaultBranch: defaultBranch,
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

func (svc *GitService) PullMain(repo *GitRepo) error {
	if repo == nil {
		return errors.New("repo is nil")
	}

	if err := svc.checkoutBranch(repo.Path, "main"); err != nil {
		return err
	}

	return svc.runGit(repo.Path, "pull")
}

func (svc *GitService) initRepo(repoPath string) error {
	if err := os.MkdirAll(repoPath, 0o775); err != nil {
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

func (svc *GitService) cloneRepo(repoURL, repoPath, token string) error {
	if strings.TrimSpace(repoURL) == "" {
		return errors.New("repo url is empty")
	}

	if err := os.MkdirAll(repoPath, 0o775); err != nil {
		return err
	}

	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return errors.New("repo path exists and is not empty")
	}

	useSSH, keyPath := gitSSHConfig()
	if useSSH {
		if strings.TrimSpace(keyPath) == "" {
			return errors.New("GITHUB_SSH_KEY_PATH not set")
		}
		repoURL = convertGitHubToSSH(repoURL)
	}

	args := []string{"clone", repoURL, repoPath}
	if !useSSH && strings.TrimSpace(token) != "" {
		encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		args = append([]string{"-c", "http.extraHeader=AUTHORIZATION: basic " + encoded}, args...)
	}

	cmd := exec.CommandContext(context.Background(), "git", args...)
	if useSSH {
		cmd.Env = append(os.Environ(),
			"GIT_SSH_COMMAND=ssh -i "+keyPath+" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new",
		)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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

func (svc *GitService) runGitOutput(repoPath string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", repoPath}, args...)...)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func (svc *GitService) defaultBranch(repoPath string) string {
	ref, err := svc.runGitOutput(repoPath, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD")
	if err == nil && ref != "" {
		parts := strings.SplitN(ref, "/", 2)
		if len(parts) == 2 && parts[1] != "" {
			return parts[1]
		}
	}

	branch, err := svc.runGitOutput(repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil && branch != "HEAD" {
		return branch
	}

	return ""
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

func gitSSHConfig() (bool, string) {
	use := isEnvTrue(os.Getenv("GITHUB_USE_SSH"))
	keyPath := strings.TrimSpace(os.Getenv("GITHUB_SSH_KEY_PATH"))
	return use, keyPath
}

func convertGitHubToSSH(repoURL string) string {
	if strings.HasPrefix(repoURL, "git@") || strings.HasPrefix(repoURL, "ssh://") {
		return repoURL
	}
	if strings.HasPrefix(repoURL, "https://github.com/") {
		path := strings.TrimPrefix(repoURL, "https://github.com/")
		path = strings.TrimPrefix(path, "/")
		return "git@github.com:" + path
	}
	if strings.HasPrefix(repoURL, "http://github.com/") {
		path := strings.TrimPrefix(repoURL, "http://github.com/")
		path = strings.TrimPrefix(path, "/")
		return "git@github.com:" + path
	}
	return repoURL
}

func boolEnv(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func isEnvTrue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}
