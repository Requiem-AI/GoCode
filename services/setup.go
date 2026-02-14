package services

import (
	"bufio"
	ctx "context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/requiem-ai/gocode/context"
	tb "gopkg.in/telebot.v3"
)

type SetupService struct {
	context.DefaultService
}

const SETUP_SVC = "setup_svc"

func (svc SetupService) Id() string {
	return SETUP_SVC
}

func (svc *SetupService) Configure(ctx *context.Context) error {
	if err := svc.DefaultService.Configure(ctx); err != nil {
		return err
	}

	if err := svc.runCodexLoginSetup(); err != nil {
		return err
	}

	if err := svc.runTelegramSetup(); err != nil {
		return err
	}

	return svc.runGithubSSHSetup()
}

func (svc *SetupService) runCodexLoginSetup() error {
	reader := bufio.NewReader(os.Stdin)

	bin := strings.TrimSpace(os.Getenv("CODEX_BIN"))
	if bin == "" {
		bin = "codex"
	}

	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("codex CLI not found (%s): %w", bin, err)
	}

	statusCtx, cancel := ctx.WithTimeout(ctx.Background(), 5*time.Second)
	defer cancel()

	statusCmd := exec.CommandContext(statusCtx, bin, "login", "status")
	out, err := statusCmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	lowerMsg := strings.ToLower(msg)

	if strings.Contains(lowerMsg, "not logged in") || strings.Contains(lowerMsg, "logged out") {
		// fallthrough to login flow
	} else if strings.Contains(lowerMsg, "logged in") {
		return nil
	}
	if err != nil && msg == "" {
		return fmt.Errorf("failed to check Codex login status: %w", err)
	}

	fmt.Fprintln(os.Stdout, "Codex login required.")
	if msg != "" {
		fmt.Fprintln(os.Stdout, msg)
	}

	if !confirm(reader, "Start Codex login now? (y/N): ") {
		return nil
	}

	loginCtx, loginCancel := ctx.WithTimeout(ctx.Background(), 5*time.Minute)
	defer loginCancel()

	loginCmd := exec.CommandContext(loginCtx, bin, "login")
	loginCmd.Stdin = os.Stdin
	loginCmd.Stdout = os.Stdout
	loginCmd.Stderr = os.Stderr
	return loginCmd.Run()
}

func (svc *SetupService) runTelegramSetup() error {
	reader := bufio.NewReader(os.Stdin)

	current := telegramConfig{
		TelegramSecret: strings.TrimSpace(os.Getenv("TELEGRAM_SECRET")),
	}

	if current.isComplete() {
		if err := svc.registerTelegramBotCommands(current.TelegramSecret); err != nil {
			return err
		}
		return svc.runTelegramUserIDSetup(current.TelegramSecret)
	}

	fmt.Fprintln(os.Stdout, "GoCode Telegram setup")
	fmt.Fprintln(os.Stdout, "Press Enter to keep the current value shown in brackets.")
	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "BotFather tips:")
	fmt.Fprintln(os.Stdout, "- Create a bot with /newbot, then copy the token.")
	fmt.Fprintln(os.Stdout, "- No webhook needed; this service uses long polling.")
	fmt.Fprintln(os.Stdout, "")

	secret, err := promptRequired(reader, "Bot token (from BotFather /newbot)", current.TelegramSecret)
	if err != nil {
		return err
	}

	next := telegramConfig{
		TelegramSecret: secret,
	}

	_ = os.Setenv("TELEGRAM_SECRET", next.TelegramSecret)

	envPath, err := envFilePath()
	if err != nil {
		return err
	}

	if err := updateEnvFile(envPath, map[string]string{
		"TELEGRAM_SECRET": next.TelegramSecret,
	}); err != nil {
		return err
	}

	fmt.Fprintln(os.Stdout, "Telegram setup saved to .env.")
	if err := svc.registerTelegramBotCommands(next.TelegramSecret); err != nil {
		return err
	}
	return svc.runTelegramUserIDSetup(next.TelegramSecret)
}

func (svc *SetupService) runGithubSSHSetup() error {
	reader := bufio.NewReader(os.Stdin)

	gitSvc := svc.Service(GIT_SVC)
	if gitSvc == nil {
		return errors.New("git service not available")
	}
	git := gitSvc.(*GitService)

	enabled := git.GitHubUseSSH()
	keyPathEnv := strings.TrimSpace(os.Getenv("GITHUB_SSH_KEY_PATH"))
	if enabled || keyPathEnv != "" {
		return nil
	}
	keyPath, err := git.GitHubSSHKeyPath()
	if err != nil {
		return err
	}

	if !confirm(reader, "Enable GitHub SSH for private repos? (y/N): ") {
		return nil
	}
	enabled = true

	if err := git.EnsureSSHKey(keyPath); err != nil {
		return err
	}

	if err := git.SetGitHubSSHConfig(keyPath, true); err != nil {
		return err
	}

	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "GitHub SSH setup")
	registered, msg, err := git.CheckGitHubSSH(keyPath)
	if err != nil {
		fmt.Fprintln(os.Stdout, "Unable to verify GitHub SSH key registration.")
		if msg != "" {
			fmt.Fprintln(os.Stdout, msg)
		}
		fmt.Fprintln(os.Stdout, err.Error())
		fmt.Fprintln(os.Stdout, "")
		return nil
	}
	if registered {
		fmt.Fprintln(os.Stdout, "SSH key is registered with GitHub.")
		if msg != "" {
			fmt.Fprintln(os.Stdout, msg)
		}
	} else {
		fmt.Fprintln(os.Stdout, "SSH key is not registered with GitHub yet.")
		fmt.Fprintln(os.Stdout, "Add your public key from:")
		fmt.Fprintln(os.Stdout, keyPath+".pub")
		fmt.Fprintln(os.Stdout, "GitHub settings: https://github.com/settings/ssh/new")
		if msg != "" {
			fmt.Fprintln(os.Stdout, msg)
		}
	}
	fmt.Fprintln(os.Stdout, "")

	return nil
}

type telegramConfig struct {
	TelegramSecret string
}

func (cfg telegramConfig) isComplete() bool {
	return cfg.TelegramSecret != ""
}

func (svc *SetupService) runTelegramUserIDSetup(secret string) error {
	if strings.TrimSpace(os.Getenv("USER_ID")) != "" {
		return nil
	}
	if strings.TrimSpace(secret) == "" {
		return errors.New("telegram bot token is required before USER_ID setup")
	}

	code, err := svc.generateTelegramVerificationCode()
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "Telegram user verification")
	fmt.Fprintln(os.Stdout, "Send this code to the bot in Telegram to authorize your user:")
	fmt.Fprintln(os.Stdout, code)
	fmt.Fprintln(os.Stdout, "")

	userID, err := svc.waitForTelegramVerification(secret, code, 5*time.Minute)
	if err != nil {
		return err
	}

	_ = os.Setenv("USER_ID", strconv.FormatInt(userID, 10))

	envPath, err := envFilePath()
	if err != nil {
		return err
	}

	if err := updateEnvFile(envPath, map[string]string{
		"USER_ID": strconv.FormatInt(userID, 10),
	}); err != nil {
		return err
	}

	fmt.Fprintln(os.Stdout, "USER_ID saved to .env.")
	return nil
}

func (svc *SetupService) registerTelegramBotCommands(secret string) error {
	if strings.TrimSpace(secret) == "" {
		return errors.New("telegram bot token is required to register commands")
	}

	bot, err := tb.NewBot(tb.Settings{
		Token:  secret,
		Poller: &tb.LongPoller{Timeout: 1 * time.Second},
	})
	if err != nil {
		return err
	}

	commands := []tb.Command{
		{Text: "start", Description: "Show quick start instructions"},
		{Text: "new", Description: "Create a topic: /new <name> [repo]"},
		{Text: "clear", Description: "Clear the current topic context"},
		{Text: "delete", Description: "Delete the current topic and repo"},
		{Text: "github", Description: "Configure GitHub auth (/github ssh|status|logout)"},
		{Text: "preview", Description: "Start/stop web preview (/preview [start|status|stop])"},
	}

	if err := bot.SetCommands(commands, tb.CommandScope{Type: tb.CommandScopeDefault}); err != nil {
		return err
	}

	fmt.Fprintln(os.Stdout, "Telegram commands and menu updated.")
	return nil
}

func (svc *SetupService) generateTelegramVerificationCode() (string, error) {
	const codeDigits = 6
	const maxDigit = 10

	var sb strings.Builder
	sb.Grow(codeDigits)
	for i := 0; i < codeDigits; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(maxDigit))
		if err != nil {
			return "", err
		}
		sb.WriteString(strconv.Itoa(int(n.Int64())))
	}
	return sb.String(), nil
}

func (svc *SetupService) waitForTelegramVerification(secret, code string, timeout time.Duration) (int64, error) {
	bot, err := tb.NewBot(tb.Settings{
		Token:  secret,
		Poller: &tb.LongPoller{Timeout: 10 * time.Second},
	})
	if err != nil {
		return 0, err
	}

	done := make(chan int64, 1)
	bot.Handle(tb.OnText, func(c tb.Context) error {
		if strings.TrimSpace(c.Text()) != code {
			return nil
		}
		sender := c.Sender()
		if sender == nil {
			return nil
		}
		select {
		case done <- sender.ID:
		default:
		}
		_ = c.Send("Verification received. You can return to the setup.")
		return nil
	})

	go bot.Start()
	defer bot.Stop()

	select {
	case userID := <-done:
		return userID, nil
	case <-time.After(timeout):
		return 0, errors.New("telegram verification timed out")
	}
}

func confirm(reader *bufio.Reader, prompt string) bool {
	fmt.Fprint(os.Stdout, prompt)
	text, _ := reader.ReadString('\n')
	text = strings.TrimSpace(strings.ToLower(text))
	return text == "y" || text == "yes"
}

func promptRequired(reader *bufio.Reader, label, current string) (string, error) {
	for {
		value, err := promptWithDefault(reader, label, current, "")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(value) == "" {
			fmt.Fprintln(os.Stdout, "Value required.")
			continue
		}
		return value, nil
	}
}

func promptWithDefault(reader *bufio.Reader, label, current, fallback string) (string, error) {
	display := current
	if display == "" {
		display = fallback
	}

	if display != "" {
		fmt.Fprintf(os.Stdout, "%s [%s]: ", label, display)
	} else {
		fmt.Fprintf(os.Stdout, "%s: ", label)
	}

	text, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	text = strings.TrimSpace(text)
	if text == "" {
		if current != "" {
			return current, nil
		}
		return fallback, nil
	}

	return text, nil
}

func envFilePath() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	return filepath.Join(wd, ".env"), nil
}

func updateEnvFile(path string, updates map[string]string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	lines := []string{}
	seen := make(map[string]bool, len(updates))

	scanner := bufio.NewScanner(strings.NewReader(string(existing)))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			lines = append(lines, line)
			continue
		}

		prefix, key := parseEnvKey(trimmed)
		if key == "" {
			lines = append(lines, line)
			continue
		}

		if value, ok := updates[key]; ok {
			lines = append(lines, fmt.Sprintf("%s%s=%s", prefix, key, formatEnvValue(value)))
			seen[key] = true
			continue
		}

		lines = append(lines, line)
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if seen[key] {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s=%s", key, formatEnvValue(updates[key])))
	}

	output := strings.Join(lines, "\n")
	if output != "" && !strings.HasSuffix(output, "\n") {
		output += "\n"
	}

	return os.WriteFile(path, []byte(output), 0o600)
}

func parseEnvKey(line string) (string, string) {
	trimmed := strings.TrimSpace(line)
	prefix := ""
	if strings.HasPrefix(trimmed, "export ") {
		prefix = "export "
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
	}

	idx := strings.Index(trimmed, "=")
	if idx <= 0 {
		return "", ""
	}

	key := strings.TrimSpace(trimmed[:idx])
	if key == "" {
		return "", ""
	}

	return prefix, key
}

func formatEnvValue(value string) string {
	if value == "" {
		return "\"\""
	}

	if !strings.ContainsAny(value, " \t#\"\\") {
		return value
	}

	escaped := strings.ReplaceAll(value, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return fmt.Sprintf("\"%s\"", escaped)
}
