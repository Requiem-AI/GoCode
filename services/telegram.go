package services

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/requiem-ai/gocode/context"
	"github.com/rs/zerolog/log"
	tb "gopkg.in/telebot.v3"
)

type TelegramService struct {
	context.DefaultService

	Bot *tb.Bot

	git   *GitService
	agent *AgentService

	mu            sync.Mutex
	topicContexts map[string]*TopicContext

	deleteTopicMarkup  *tb.ReplyMarkup
	deleteTopicConfirm tb.Btn
	deleteTopicCancel  tb.Btn
}

type TopicContext struct {
	Messages []string
}

const TELEGRAM_SVC = "telegram_svc"

func (svc TelegramService) Id() string {
	return TELEGRAM_SVC
}

func (svc *TelegramService) Configure(ctx *context.Context) (err error) {
	svc.Bot, err = tb.NewBot(tb.Settings{
		Token:  os.Getenv("TELEGRAM_SECRET"),
		Poller: &tb.LongPoller{},
	})
	if err != nil {
		return err
	}

	svc.topicContexts = make(map[string]*TopicContext)

	return svc.DefaultService.Configure(ctx)
}

func (svc *TelegramService) Start() error {
	svc.agent = svc.Service(Agent_SVC).(*AgentService)
	svc.git = svc.Service(GIT_SVC).(*GitService)

	svc.setupHandlers()
	svc.setupEvents()

	svc.Bot.Start()

	return nil
}

func (svc *TelegramService) Shutdown() {
	if svc.Bot == nil {
		return
	}
	svc.Bot.Stop()
}

func (svc *TelegramService) setupHandlers() {
	svc.Bot.Handle("/start", svc.onStart)
	svc.Bot.Handle("/clear", svc.onClear)
	svc.Bot.Handle("/new", svc.onTopic)
	svc.Bot.Handle("/delete", svc.onDeleteTopic)
	svc.Bot.Handle("/github", svc.onGithub)

	svc.Bot.Handle(tb.OnText, svc.onText)

	svc.deleteTopicMarkup = &tb.ReplyMarkup{}
	svc.deleteTopicConfirm = svc.deleteTopicMarkup.Data("Delete", "topic_delete_confirm")
	svc.deleteTopicCancel = svc.deleteTopicMarkup.Data("Cancel", "topic_delete_cancel")
	svc.deleteTopicMarkup.Inline(
		svc.deleteTopicMarkup.Row(svc.deleteTopicConfirm, svc.deleteTopicCancel),
	)

	svc.Bot.Handle(&svc.deleteTopicConfirm, svc.onDeleteTopicConfirm)
	svc.Bot.Handle(&svc.deleteTopicCancel, svc.onDeleteTopicCancel)
}

func (svc *TelegramService) setupEvents() {
	svc.Bot.Handle(tb.OnTopicCreated, svc.onTopicCreated)
}

func (svc *TelegramService) onText(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		return nil
	}

	if strings.HasPrefix(msg.Text, "/") {
		return nil
	}

	if !msg.TopicMessage || msg.ThreadID == 0 {
		log.Info().Str("text", msg.Text).Msg("onText main")

		_ = svc.Bot.React(c.Chat(), c.Message(), tb.ReactionOptions{Reactions: []tb.Reaction{tb.Reaction{
			Type:  "emoji",
			Emoji: "👍",
		}}})

		resp, err := svc.agent.Run("", c.Text())
		if err != nil {
			log.Error().Err(err).Msg("failed to run agent request (main)")
			return c.Send("Agent failed to run.")
		}

		_ = svc.Bot.Notify(c.Chat(), tb.Typing)

		_, err = svc.Bot.Send(c.Chat(),
			resp,
			&tb.SendOptions{ParseMode: tb.ModeMarkdown})
		return err
	}

	log.Info().Str("text", msg.Text).Int("topic", msg.ThreadID).Msg("onText topic")

	repo, err := svc.ensureRepo(c.Chat(), msg.ThreadID)
	if err != nil {
		log.Error().Err(err).Msg("failed to ensure repo")
		return c.Send("Couldn't prepare the repo for this topic.")
	}

	//branch, err := svc.createFeatureBranch(repo, msg.Text)
	//if err != nil {
	//	log.Error().Err(err).Msg("failed to create feature branch")
	//	return c.Send("Couldn't prepare a feature branch.")
	//}

	_ = svc.Bot.React(c.Chat(), c.Message(), tb.ReactionOptions{Reactions: []tb.Reaction{tb.Reaction{
		Type:  "emoji",
		Emoji: "👍",
	}}})

	resp, err := svc.agent.Run(repo.Path, c.Text())
	if err != nil {
		log.Error().Err(err).Msg("failed to run agent request")
		return c.Send("Agent failed to run.")
	}

	_ = svc.Bot.Notify(c.Chat(), tb.Typing, msg.ThreadID)

	_, err = svc.Bot.Send(c.Chat(),
		resp,
		&tb.SendOptions{ThreadID: msg.ThreadID, ParseMode: tb.ModeMarkdown})
	return err
}

func (svc *TelegramService) onStart(c tb.Context) error {
	log.Info().Str("text", c.Text()).Msg("onStart")
	return c.Send("Create a topic with /new <name> [repo-url|repo-path]. Use /github ssh first for private repos.")
}

func (svc *TelegramService) onClear(c tb.Context) error {
	msg := c.Message()
	if msg == nil || !msg.TopicMessage || msg.ThreadID == 0 {
		return c.Send("Use /clear inside a topic to reset the context.")
	}

	log.Info().Int("topic", msg.ThreadID).Msg("onClear")

	repo, err := svc.ensureRepo(c.Chat(), msg.ThreadID)
	if err != nil {
		log.Error().Err(err).Msg("failed to ensure repo for clear")
		return c.Send("Couldn't prepare the repo for this topic.")
	}

	if err := svc.agent.Clear(repo.Path); err != nil {
		log.Error().Err(err).Msg("failed to clear agent context")
		return c.Send("Failed to clear the context.", &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	_, err := svc.Bot.Send(c.Chat(), "Context cleared.", &tb.SendOptions{ThreadID: msg.ThreadID})
	return err
}

func (svc *TelegramService) onDeleteTopic(c tb.Context) error {
	msg := c.Message()
	if msg == nil || !msg.TopicMessage || msg.ThreadID == 0 {
		return c.Send("Use /delete inside the topic you want to remove.")
	}

	_, err := svc.Bot.Send(c.Chat(),
		"This will delete the topic and its repository. Are you sure?",
		&tb.SendOptions{ThreadID: msg.ThreadID, ReplyMarkup: svc.deleteTopicMarkup})
	return err
}

func (svc *TelegramService) onDeleteTopicConfirm(c tb.Context) error {
	msg := c.Message()
	if msg == nil || !msg.TopicMessage || msg.ThreadID == 0 {
		return c.Respond(&tb.CallbackResponse{Text: "Use /delete inside a topic.", ShowAlert: true})
	}

	_ = c.Respond()

	if err := svc.deleteTopicRepo(c.Chat(), msg.ThreadID); err != nil {
		log.Error().Err(err).Msg("failed to delete repo for topic")
		return c.Send(fmt.Sprintf("Failed to delete repo: %s", err.Error()), &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	if err := svc.Bot.DeleteTopic(c.Chat(), &tb.Topic{ThreadID: msg.ThreadID}); err != nil {
		log.Error().Err(err).Msg("failed to delete topic")
		return c.Send(fmt.Sprintf("Failed to delete topic: %s", err.Error()), &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	return nil
}

func (svc *TelegramService) onDeleteTopicCancel(c tb.Context) error {
	_ = c.Respond()
	_, err := svc.Bot.Edit(c.Message(), "Deletion cancelled.")
	return err
}

func (svc *TelegramService) onTopic(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		return nil
	}

	name, repoURL, repoPath := svc.parseTopicArgs(msg.Payload)
	if name == "" {
		return c.Send("Usage: /new <name> [repo-url|repo-path]")
	}

	topic, err := svc.Bot.CreateTopic(c.Chat(), &tb.Topic{
		Name: name,
	})
	if err != nil {
		log.Error().Err(err).Msg("failed to create topic")
		return c.Send("Couldn't create the topic.")
	}

	token := svc.git.GitHubToken()
	repo, err := svc.ensureRepoFrom(c.Chat(), topic.ThreadID, repoURL, repoPath, token)
	if err != nil {
		log.Error().Err(err).Msg("failed to create repo")
		if repoURL != "" || repoPath != "" {
			if delErr := svc.Bot.DeleteTopic(c.Chat(), topic); delErr != nil {
				log.Error().Err(delErr).Msg("failed to delete topic after repo error")
			}
			return c.Send(fmt.Sprintf("Repo setup failed: %s", err.Error()))
		}
		return c.Send(fmt.Sprintf("Topic created, but repo creation failed: %s", err.Error()))
	}

	_, err = svc.Bot.Send(c.Chat(),
		fmt.Sprintf("Topic ready. Repo: `%s`", repo.Path),
		&tb.SendOptions{ThreadID: topic.ThreadID, ParseMode: tb.ModeMarkdown})
	return err
}

func (svc *TelegramService) onTopicCreated(c tb.Context) error {
	topic := c.Topic()
	if topic == nil {
		return nil
	}

	repo, err := svc.ensureRepo(c.Chat(), topic.ThreadID)
	if err != nil {
		log.Error().Err(err).Msg("failed to create repo for topic")
		return nil
	}

	_, err = svc.Bot.Send(c.Chat(),
		fmt.Sprintf("Repo initialized for this topic: `%s`", repo.Path),
		&tb.SendOptions{ThreadID: topic.ThreadID, ParseMode: tb.ModeMarkdown})
	return err
}

func (svc *TelegramService) ensureRepo(chat *tb.Chat, threadID int) (*GitRepo, error) {
	if svc.git == nil {
		return nil, errors.New("git service not available")
	}

	if chat == nil {
		return nil, errors.New("missing chat")
	}

	return svc.git.EnsureTopicRepo(chat.ID, threadID)
}

func (svc *TelegramService) ensureRepoFrom(chat *tb.Chat, threadID int, repoURL, repoPath, token string) (*GitRepo, error) {
	if svc.git == nil {
		return nil, errors.New("git service not available")
	}

	if chat == nil {
		return nil, errors.New("missing chat")
	}

	if strings.TrimSpace(repoPath) != "" {
		return svc.git.EnsureTopicRepoFromPath(chat.ID, threadID, repoPath)
	}

	if repoURL == "" {
		return svc.git.EnsureTopicRepo(chat.ID, threadID)
	}

	return svc.git.EnsureTopicRepoFrom(chat.ID, threadID, repoURL, token)
}

func (svc *TelegramService) deleteTopicRepo(chat *tb.Chat, threadID int) error {
	if svc.git == nil {
		return errors.New("git service not available")
	}
	if chat == nil {
		return errors.New("missing chat")
	}

	return svc.git.DeleteTopicRepo(chat.ID, threadID)
}

func (svc *TelegramService) createFeatureBranch(repo *GitRepo, feature string) (string, error) {
	if svc.git == nil {
		return "", errors.New("git service not available")
	}

	return svc.git.CreateFeatureBranch(repo, feature)
}

func (svc *TelegramService) parseTopicArgs(payload string) (string, string, string) {
	fields := strings.Fields(payload)
	if len(fields) == 0 {
		return "", "", ""
	}
	if len(fields) == 1 {
		if svc.looksLikeRepoURL(fields[0]) {
			return svc.topicNameFromRepoURL(fields[0]), fields[0], ""
		}
		if svc.looksLikeRepoPath(fields[0]) {
			return svc.topicNameFromRepoPath(fields[0]), "", fields[0]
		}
		return fields[0], "", ""
	}
	if svc.looksLikeRepoURL(fields[1]) {
		return fields[0], fields[1], ""
	}
	if svc.looksLikeRepoPath(fields[1]) {
		return fields[0], "", fields[1]
	}
	return fields[0], fields[1], ""
}

func (svc *TelegramService) looksLikeRepoURL(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.Contains(value, "://") || strings.HasPrefix(value, "git@") || strings.HasPrefix(value, "ssh://") {
		return true
	}
	if strings.HasSuffix(value, ".git") {
		return true
	}
	return strings.Contains(value, "github.com/") || strings.Contains(value, "gitlab.com/") || strings.Contains(value, "bitbucket.org/")
}

func (svc *TelegramService) looksLikeRepoPath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.Contains(value, "://") || strings.HasPrefix(value, "git@") || strings.HasPrefix(value, "ssh://") {
		return false
	}
	if strings.HasPrefix(value, "~") || strings.HasPrefix(value, ".") || strings.HasPrefix(value, "/") {
		return true
	}
	return strings.Contains(value, string(filepath.Separator))
}

func (svc *TelegramService) topicNameFromRepoURL(repoURL string) string {
	trimmed := strings.TrimSpace(repoURL)
	trimmed = strings.TrimSuffix(trimmed, "/")
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err == nil && parsed.Path != "" {
			trimmed = parsed.Path
		}
	}
	if strings.Contains(trimmed, ":") {
		parts := strings.Split(trimmed, ":")
		trimmed = parts[len(parts)-1]
	}
	if strings.Contains(trimmed, "/") {
		segments := strings.Split(trimmed, "/")
		trimmed = segments[len(segments)-1]
	}
	return strings.TrimSuffix(trimmed, ".git")
}

func (svc *TelegramService) topicNameFromRepoPath(repoPath string) string {
	trimmed := strings.TrimSpace(repoPath)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			trimmed = filepath.Join(home, strings.TrimPrefix(trimmed, "~"))
		}
	}
	trimmed = filepath.Clean(trimmed)
	base := filepath.Base(trimmed)
	if base == "." || base == string(filepath.Separator) {
		return "repo"
	}
	return base
}

func (svc *TelegramService) onGithub(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		return nil
	}

	payload := strings.TrimSpace(msg.Payload)
	if payload == "" {
		return c.Send("Usage: /github ssh | /github status | /github logout")
	}

	switch {
	case strings.EqualFold(payload, "login"):
		return svc.startGithubSSH(c)
	case strings.EqualFold(payload, "ssh"):
		return svc.startGithubSSH(c)
	case strings.EqualFold(payload, "logout") || strings.EqualFold(payload, "clear"):
		if err := svc.git.ClearGitHubAuth(); err != nil {
			log.Error().Err(err).Msg("failed to clear github auth")
			return c.Send("Failed to clear GitHub auth.")
		}
		return c.Send("GitHub auth cleared.")
	case strings.EqualFold(payload, "status"):
		if svc.git.GitHubUseSSH() {
			return c.Send("GitHub SSH is enabled.")
		}
		if token := svc.git.GitHubToken(); token != "" {
			return c.Send("GitHub token is set (HTTPS).")
		}
		return c.Send("GitHub token is not set.")
	}

	if err := svc.git.SetGitHubToken(payload); err != nil {
		log.Error().Err(err).Msg("failed to save github token")
		return c.Send("Failed to save GitHub token.")
	}
	return c.Send("GitHub token saved.")
}

func (svc *TelegramService) startGithubSSH(c tb.Context) error {
	keyPath, err := svc.git.GitHubSSHKeyPath()
	if err != nil {
		return c.Send("Could not determine home directory for SSH key.")
	}

	if err := svc.git.EnsureSSHKey(keyPath); err != nil {
		log.Error().Err(err).Msg("failed to ensure ssh key")
		return c.Send("Failed to create SSH key.")
	}

	pubPath := keyPath + ".pub"
	pubKey, err := os.ReadFile(pubPath)
	if err != nil {
		log.Error().Err(err).Msg("failed to read ssh public key")
		return c.Send("Failed to read SSH public key.")
	}

	if err := svc.git.SetGitHubSSHConfig(keyPath, true); err != nil {
		log.Error().Err(err).Msg("failed to save ssh config")
		return c.Send("Failed to save SSH config.")
	}

	msg := fmt.Sprintf("SSH key ready. Add this public key to GitHub:\n`%s`", strings.TrimSpace(string(pubKey)))
	_, err = svc.Bot.Send(c.Chat(), msg)
	return err
}
