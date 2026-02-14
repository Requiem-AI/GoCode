package services

import (
	"bufio"
	ctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/requiem-ai/gocode/context"
	"github.com/rs/zerolog/log"
)

type PreviewService struct {
	context.DefaultService

	mu       sync.Mutex
	sessions map[string]*PreviewSession

	devURLRe   *regexp.Regexp
	portLineRe *regexp.Regexp
}

type PreviewSession struct {
	ChatID   int64
	ThreadID int
	RepoPath string

	Tunnel string
	URL    string
	Port   int

	DevCmd    *exec.Cmd
	DevCancel ctx.CancelFunc
	DevExitCh <-chan error

	TunnelCmd    *exec.Cmd
	TunnelCancel ctx.CancelFunc
}

const PREVIEW_SVC = "preview_svc"

func (svc PreviewService) Id() string {
	return PREVIEW_SVC
}

func (svc *PreviewService) Configure(ctx *context.Context) error {
	if err := svc.DefaultService.Configure(ctx); err != nil {
		return err
	}

	svc.sessions = make(map[string]*PreviewSession)
	svc.devURLRe = regexp.MustCompile(`http://(?:localhost|127\\.0\\.0\\.1|0\\.0\\.0\\.0|\\[::1\\]):(\\d+)`)
	svc.portLineRe = regexp.MustCompile(`(?i)\\b(?:port|listening)\\b[^0-9]*(\\d{2,5})`)
	return nil
}

func (svc *PreviewService) Start() error {
	return nil
}

func (svc *PreviewService) Shutdown() {
	svc.mu.Lock()
	keys := make([]string, 0, len(svc.sessions))
	for key := range svc.sessions {
		keys = append(keys, key)
	}
	svc.mu.Unlock()

	for _, key := range keys {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			continue
		}
		chatID, _ := strconv.ParseInt(parts[0], 10, 64)
		threadID, _ := strconv.Atoi(parts[1])
		_ = svc.StopPreview(chatID, threadID)
	}
}

func (svc *PreviewService) StartPreview(chatID int64, threadID int, repoPath string, tunnelOverride string) (*PreviewSession, error) {
	if repoPath == "" {
		return nil, errors.New("repo path is empty")
	}
	key := topicKey(chatID, threadID)

	svc.mu.Lock()
	if session := svc.sessions[key]; session != nil {
		svc.mu.Unlock()
		return session, nil
	}
	svc.mu.Unlock()

	port, devCmd, devCancel, devExitCh, err := svc.startDevServer(repoPath)
	if err != nil {
		return nil, err
	}

	tunnel, err := svc.pickTunnel(tunnelOverride)
	if err != nil {
		devCancel()
		_ = devCmd.Process.Kill()
		return nil, err
	}

	url, tunnelCmd, tunnelCancel, err := svc.startTunnel(tunnel, port)
	if err != nil {
		devCancel()
		_ = devCmd.Process.Kill()
		return nil, err
	}

	session := &PreviewSession{
		ChatID:    chatID,
		ThreadID:  threadID,
		RepoPath:  repoPath,
		Tunnel:    tunnel,
		URL:       url,
		Port:      port,
		DevCmd:    devCmd,
		DevExitCh: devExitCh,
		DevCancel: func() {
			devCancel()
		},
		TunnelCmd:    tunnelCmd,
		TunnelCancel: tunnelCancel,
	}

	svc.mu.Lock()
	svc.sessions[key] = session
	svc.mu.Unlock()

	go svc.monitorSession(session)

	return session, nil
}

func (svc *PreviewService) StopPreview(chatID int64, threadID int) error {
	key := topicKey(chatID, threadID)
	var session *PreviewSession

	svc.mu.Lock()
	session = svc.sessions[key]
	delete(svc.sessions, key)
	svc.mu.Unlock()

	if session == nil {
		return nil
	}

	if session.TunnelCancel != nil {
		session.TunnelCancel()
	}
	if session.TunnelCmd != nil && session.TunnelCmd.Process != nil {
		_ = session.TunnelCmd.Process.Kill()
	}

	if session.DevCancel != nil {
		session.DevCancel()
	}
	if session.DevCmd != nil && session.DevCmd.Process != nil {
		_ = session.DevCmd.Process.Kill()
	}

	if session.Tunnel == "tailscale" {
		svc.stopTailscaleFunnel()
	}

	return nil
}

func (svc *PreviewService) PreviewStatus(chatID int64, threadID int) (*PreviewSession, bool) {
	key := topicKey(chatID, threadID)
	svc.mu.Lock()
	session := svc.sessions[key]
	svc.mu.Unlock()
	if session == nil {
		return nil, false
	}
	return session, true
}

func (svc *PreviewService) monitorSession(session *PreviewSession) {
	if session == nil || session.DevExitCh == nil {
		return
	}

	err, ok := <-session.DevExitCh
	if ok && err != nil {
		log.Warn().Err(err).Str("repo", session.RepoPath).Msg("preview dev server exited")
	}
	_ = svc.StopPreview(session.ChatID, session.ThreadID)
}

func (svc *PreviewService) startDevServer(repoPath string) (int, *exec.Cmd, ctx.CancelFunc, <-chan error, error) {
	if _, err := os.Stat(filepath.Join(repoPath, "package.json")); err != nil {
		return 0, nil, nil, nil, errors.New("package.json not found; unable to run yarn dev")
	}

	devCtx, devCancel := ctx.WithCancel(ctx.Background())
	cmd := exec.CommandContext(devCtx, "yarn", "dev")
	cmd.Dir = repoPath

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		devCancel()
		return 0, nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		devCancel()
		return 0, nil, nil, nil, err
	}

	if err := cmd.Start(); err != nil {
		devCancel()
		return 0, nil, nil, nil, err
	}

	portCh := make(chan int, 1)
	errCh := make(chan error, 1)
	lines := make(chan string, 64)

	var wg sync.WaitGroup
	wg.Add(2)
	go svc.scanOutput(lines, stdout, &wg)
	go svc.scanOutput(lines, stderr, &wg)
	go func() {
		wg.Wait()
		close(lines)
	}()

	exitCh := make(chan error, 1)
	go func() {
		exitCh <- cmd.Wait()
		close(exitCh)
	}()

	go func() {
		for line := range lines {
			if port := svc.extractPort(line); port != 0 {
				select {
				case portCh <- port:
				default:
				}
				return
			}
		}
	}()

	go func() {
		if err := <-exitCh; err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}()

	select {
	case port := <-portCh:
		return port, cmd, devCancel, exitCh, nil
	case err := <-errCh:
		devCancel()
		return 0, nil, nil, nil, fmt.Errorf("dev server exited early: %w", err)
	case <-time.After(20 * time.Second):
		devCancel()
		_ = cmd.Process.Kill()
		return 0, nil, nil, nil, errors.New("timed out waiting for dev server port")
	}
}

func (svc *PreviewService) scanOutput(lines chan<- string, reader io.Reader, wg *sync.WaitGroup) {
	defer wg.Done()

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		text := scanner.Text()
		select {
		case lines <- text:
		default:
		}
	}
}

func (svc *PreviewService) extractPort(line string) int {
	if svc.devURLRe != nil {
		if matches := svc.devURLRe.FindStringSubmatch(line); len(matches) == 2 {
			if port, err := strconv.Atoi(matches[1]); err == nil {
				return port
			}
		}
	}
	if svc.portLineRe != nil {
		if matches := svc.portLineRe.FindStringSubmatch(line); len(matches) == 2 {
			if port, err := strconv.Atoi(matches[1]); err == nil {
				return port
			}
		}
	}
	return 0
}

func (svc *PreviewService) pickTunnel(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		choice := strings.ToLower(strings.TrimSpace(override))
		if choice == "ngrok" || choice == "tailscale" {
			return choice, nil
		}
		return "", fmt.Errorf("unknown tunnel %q", override)
	}

	if tunnel := strings.ToLower(strings.TrimSpace(os.Getenv("PREVIEW_TUNNEL"))); tunnel != "" {
		if tunnel == "ngrok" || tunnel == "tailscale" {
			return tunnel, nil
		}
		return "", fmt.Errorf("unknown PREVIEW_TUNNEL %q", tunnel)
	}

	if _, err := exec.LookPath("ngrok"); err == nil {
		return "ngrok", nil
	}
	if _, err := exec.LookPath("tailscale"); err == nil {
		return "tailscale", nil
	}

	return "", errors.New("no tunnel binary found (install ngrok or tailscale)")
}

func (svc *PreviewService) startTunnel(tunnel string, port int) (string, *exec.Cmd, ctx.CancelFunc, error) {
	switch tunnel {
	case "ngrok":
		return svc.startNgrokTunnel(port)
	case "tailscale":
		return svc.startTailscaleFunnel(port)
	default:
		return "", nil, nil, fmt.Errorf("unknown tunnel %q", tunnel)
	}
}

func (svc *PreviewService) startNgrokTunnel(port int) (string, *exec.Cmd, ctx.CancelFunc, error) {
	ngrokBin := strings.TrimSpace(os.Getenv("NGROK_BIN"))
	if ngrokBin == "" {
		ngrokBin = "ngrok"
	}
	if _, err := exec.LookPath(ngrokBin); err != nil {
		return "", nil, nil, fmt.Errorf("ngrok not found: %w", err)
	}

	ngCtx, ngCancel := ctx.WithCancel(ctx.Background())
	cmd := exec.CommandContext(ngCtx, ngrokBin, "http", "--log=stdout", "--log-format=json", strconv.Itoa(port))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		ngCancel()
		return "", nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		ngCancel()
		return "", nil, nil, err
	}

	if err := cmd.Start(); err != nil {
		ngCancel()
		return "", nil, nil, err
	}

	urlCh := make(chan string, 1)
	errCh := make(chan error, 1)
	lines := make(chan string, 64)

	var wg sync.WaitGroup
	wg.Add(2)
	go svc.scanOutput(lines, stdout, &wg)
	go svc.scanOutput(lines, stderr, &wg)
	go func() {
		wg.Wait()
		close(lines)
	}()

	go func() {
		for line := range lines {
			url := svc.extractNgrokURL(line)
			if url == "" {
				continue
			}
			select {
			case urlCh <- url:
			default:
			}
			return
		}
	}()

	go func() {
		err := cmd.Wait()
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}()

	select {
	case url := <-urlCh:
		return url, cmd, ngCancel, nil
	case err := <-errCh:
		ngCancel()
		return "", nil, nil, fmt.Errorf("ngrok exited early: %w", err)
	case <-time.After(20 * time.Second):
		ngCancel()
		_ = cmd.Process.Kill()
		return "", nil, nil, errors.New("timed out waiting for ngrok url")
	}
}

func (svc *PreviewService) extractNgrokURL(line string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		return ""
	}
	if url, ok := payload["url"].(string); ok && strings.HasPrefix(url, "https://") {
		return url
	}
	if payload["msg"] == "started tunnel" {
		if url, ok := payload["url"].(string); ok && strings.HasPrefix(url, "https://") {
			return url
		}
	}
	return ""
}

func (svc *PreviewService) startTailscaleFunnel(port int) (string, *exec.Cmd, ctx.CancelFunc, error) {
	tailscaleBin := strings.TrimSpace(os.Getenv("TAILSCALE_BIN"))
	if tailscaleBin == "" {
		tailscaleBin = "tailscale"
	}
	if _, err := exec.LookPath(tailscaleBin); err != nil {
		return "", nil, nil, fmt.Errorf("tailscale not found: %w", err)
	}

	serveCmd := exec.Command(tailscaleBin, "serve", "https", "/", "http://127.0.0.1:"+strconv.Itoa(port))
	serveCmd.Stdout = os.Stdout
	serveCmd.Stderr = os.Stderr
	if err := serveCmd.Run(); err != nil {
		return "", nil, nil, fmt.Errorf("tailscale serve failed: %w", err)
	}

	funnelCmd := exec.Command(tailscaleBin, "funnel", "443", "on")
	funnelCmd.Stdout = os.Stdout
	funnelCmd.Stderr = os.Stderr
	if err := funnelCmd.Run(); err != nil {
		return "", nil, nil, fmt.Errorf("tailscale funnel failed: %w", err)
	}

	url, err := svc.tailscalePublicURL(tailscaleBin)
	if err != nil {
		return "", nil, nil, err
	}

	return url, nil, nil, nil
}

func (svc *PreviewService) tailscalePublicURL(tailscaleBin string) (string, error) {
	statusCmd := exec.Command(tailscaleBin, "status", "--json")
	out, err := statusCmd.Output()
	if err != nil {
		return "", fmt.Errorf("tailscale status failed: %w", err)
	}

	var payload struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", fmt.Errorf("failed to parse tailscale status: %w", err)
	}
	if payload.Self.DNSName == "" {
		return "", errors.New("tailscale DNS name not found in status")
	}

	return "https://" + payload.Self.DNSName + "/", nil
}

func (svc *PreviewService) stopTailscaleFunnel() {
	tailscaleBin := strings.TrimSpace(os.Getenv("TAILSCALE_BIN"))
	if tailscaleBin == "" {
		tailscaleBin = "tailscale"
	}
	if _, err := exec.LookPath(tailscaleBin); err != nil {
		return
	}

	_ = exec.Command(tailscaleBin, "funnel", "443", "off").Run()
	_ = exec.Command(tailscaleBin, "serve", "reset").Run()
}
