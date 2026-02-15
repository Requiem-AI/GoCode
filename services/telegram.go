package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/requiem-ai/gocode/context"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	tb "gopkg.in/telebot.v3"
)

type TelegramService struct {
	context.DefaultService

	Bot *tb.Bot

	git     *GitService
	agent   *AgentService
	preview *PreviewService

	mu                sync.Mutex
	topicContexts     map[string]*TopicContext
	topicContextsPath string
	allowedUserID     int64

	deleteTopicMarkup  *tb.ReplyMarkup
	deleteTopicConfirm tb.Btn
	deleteTopicCancel  tb.Btn
}

type TopicContext struct {
	Messages []string
	RepoURL  string
	RepoPath string
}

const TELEGRAM_SVC = "telegram_svc"

func (svc TelegramService) Id() string {
	return TELEGRAM_SVC
}

func (svc *TelegramService) Configure(ctx *context.Context) (err error) {
	allowedUserID, err := svc.parseAllowedUserID()
	if err != nil {
		return err
	}
	svc.allowedUserID = allowedUserID

	svc.Bot, err = tb.NewBot(tb.Settings{
		Token: os.Getenv("TELEGRAM_SECRET"),
		Poller: &tb.LongPoller{
			Timeout: 30 * time.Second,
		},
		OnError: func(err error, c tb.Context) {
			svc.decorateTelegramEvent(log.Error().Err(err), c).Msg("telegram bot error")
		},
	})
	if err != nil {
		return err
	}

	svc.topicContexts = make(map[string]*TopicContext)
	path := strings.TrimSpace(os.Getenv("TELEGRAM_TOPIC_CONTEXTS_PATH"))
	if path == "" {
		path = filepath.Join("data", "telegram_topics.json")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	svc.topicContextsPath = absPath

	return svc.DefaultService.Configure(ctx)
}

func (svc *TelegramService) Start() error {
	svc.agent = svc.Service(Agent_SVC).(*AgentService)
	svc.git = svc.Service(GIT_SVC).(*GitService)
	svc.preview = svc.Service(PREVIEW_SVC).(*PreviewService)

	if err := svc.loadTopicContexts(); err != nil {
		log.Error().Err(err).Msg("failed to load topic contexts")
	}

	svc.setupHandlers()
	svc.setupEvents()
	svc.sendOnlineMessage()

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
	svc.Bot.Handle("/clear", svc.guardHandler(svc.onClear))
	svc.Bot.Handle("/new", svc.guardHandler(svc.onTopic))
	svc.Bot.Handle("/delete", svc.guardHandler(svc.onDeleteTopic))
	svc.Bot.Handle("/github", svc.guardHandler(svc.onGithub))
	svc.Bot.Handle("/pull", svc.guardHandler(svc.onPull))
	svc.Bot.Handle("/preview", svc.guardHandler(svc.onPreview))
	svc.Bot.Handle("/branch", svc.guardHandler(svc.onBranch))
	svc.Bot.Handle("/commit", svc.guardHandler(svc.onCommit))
	svc.Bot.Handle("/restart", svc.guardHandler(svc.onRestart))

	svc.Bot.Handle(tb.OnText, svc.guardHandler(svc.onText))

	svc.deleteTopicMarkup = &tb.ReplyMarkup{}
	svc.deleteTopicConfirm = svc.deleteTopicMarkup.Data("Delete", "topic_delete_confirm")
	svc.deleteTopicCancel = svc.deleteTopicMarkup.Data("Cancel", "topic_delete_cancel")
	svc.deleteTopicMarkup.Inline(
		svc.deleteTopicMarkup.Row(svc.deleteTopicConfirm, svc.deleteTopicCancel),
	)

	svc.Bot.Handle(&svc.deleteTopicConfirm, svc.guardHandler(svc.onDeleteTopicConfirm))
	svc.Bot.Handle(&svc.deleteTopicCancel, svc.guardHandler(svc.onDeleteTopicCancel))
}

func (svc *TelegramService) setupEvents() {
	svc.Bot.Handle(tb.OnTopicCreated, svc.guardHandler(svc.onTopicCreated))
}

func (svc *TelegramService) sendOnlineMessage() {
	if svc.Bot == nil {
		return
	}

	chatID, ok := svc.mainChatID()
	if !ok {
		log.Warn().Msg("skipping online message: main chat id not configured or discoverable")
		return
	}

	message := strings.TrimSpace(os.Getenv("TELEGRAM_ONLINE_MESSAGE"))
	if message == "" {
		message = "Bot is online."
	}

	_, err := svc.Bot.Send(&tb.Chat{ID: chatID}, message)
	if err != nil {
		log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send online message")
		return
	}

	log.Info().Int64("chat_id", chatID).Msg("sent online message to main chat")
}

func (svc *TelegramService) mainChatID() (int64, bool) {
	raw := strings.TrimSpace(os.Getenv("TELEGRAM_MAIN_CHAT_ID"))
	if raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			log.Error().Err(err).Str("value", raw).Msg("invalid TELEGRAM_MAIN_CHAT_ID")
			return 0, false
		}
		return value, true
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()
	for key := range svc.topicContexts {
		chatID, _, ok := parseTopicKey(key)
		if ok {
			return chatID, true
		}
	}

	return 0, false
}

func (svc *TelegramService) guardHandler(fn tb.HandlerFunc) tb.HandlerFunc {
	return func(c tb.Context) error {
		if c != nil {
			svc.decorateTelegramEvent(log.Info(), c).Msg("inbound telegram update")
		}

		allowed, reason := svc.isAllowedUser(c)
		if !allowed {
			svc.decorateTelegramEvent(
				log.Warn().
					Str("reason", reason).
					Int64("allowed_user_id", svc.allowedUserID),
				c,
			).Msg("telegram update blocked")
			return nil
		}

		if err := fn(c); err != nil {
			svc.decorateTelegramEvent(log.Error().Err(err), c).Msg("telegram handler returned error")
			return err
		}

		return nil
	}
}

func (svc *TelegramService) decorateTelegramEvent(event *zerolog.Event, c tb.Context) *zerolog.Event {
	if event == nil || c == nil {
		return event
	}

	if chat := c.Chat(); chat != nil {
		event = event.Int64("group_id", chat.ID).Str("chat_type", string(chat.Type))
	}

	if sender := c.Sender(); sender != nil {
		event = event.Int64("user_id", sender.ID).Str("sender_username", sender.Username)
	}

	if msg := c.Message(); msg != nil {
		text := strings.TrimSpace(msg.Text)
		command := ""
		if strings.HasPrefix(text, "/") {
			fields := strings.Fields(strings.TrimPrefix(text, "/"))
			if len(fields) > 0 {
				command = fields[0]
			}
		}

		event = event.
			Int("thread_id", msg.ThreadID).
			Bool("topic_message", msg.TopicMessage).
			Str("message_text", msg.Text).
			Str("message_payload", msg.Payload)
		if command != "" {
			event = event.Str("command", command)
		}
	}

	if callback := c.Callback(); callback != nil {
		event = event.
			Str("callback_data", callback.Data).
			Str("callback_unique", callback.Unique)
	}

	return event
}

func (svc *TelegramService) isAllowedUser(c tb.Context) (bool, string) {
	if svc.allowedUserID == 0 {
		return true, ""
	}
	if c == nil {
		return false, "missing_context"
	}
	sender := c.Sender()
	if sender == nil {
		return false, "missing_sender"
	}
	if sender.ID == svc.Bot.Me.ID {
		return false, "sender_is_bot" // Ignore bot msgs
	}
	if sender.ID != svc.allowedUserID {
		return false, "sender_not_allowed"
	}
	return true, ""
}

func (svc *TelegramService) parseAllowedUserID() (int64, error) {
	raw := strings.TrimSpace(os.Getenv("USER_ID"))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid USER_ID %q: %w", raw, err)
	}
	return value, nil
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

	_, err = svc.Bot.Send(c.Chat(), "Context cleared.", &tb.SendOptions{ThreadID: msg.ThreadID})
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

	svc.deleteTopicContext(c.Chat().ID, msg.ThreadID)

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
	if repoURL != "" || repoPath != "" {
		if existingThreadID, ok := svc.findTopicForRepo(repoURL, repoPath); ok {
			return c.Send(fmt.Sprintf("Repo already linked to topic %d. Use that topic or delete it before creating another.", existingThreadID))
		}
	}

	topic, err := svc.Bot.CreateTopic(c.Chat(), &tb.Topic{
		Name: name,
	})
	if err != nil {
		log.Error().Err(err).Msg("failed to create topic")
		return c.Send("Couldn't create the topic.")
	}

	token := svc.git.GitHubToken()
	_, err = svc.ensureRepoFrom(c.Chat(), topic.ThreadID, repoURL, repoPath, token)
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

	if repoURL != "" || repoPath != "" {
		svc.setTopicContext(c.Chat().ID, topic.ThreadID, &TopicContext{
			RepoURL:  repoURL,
			RepoPath: repoPath,
		})
	}

	_, err = svc.Bot.Send(c.Chat(),
		"Topic ready. Type anything to start",
		&tb.SendOptions{ThreadID: topic.ThreadID, ParseMode: tb.ModeMarkdown})
	return err
}

func (svc *TelegramService) onTopicCreated(c tb.Context) error {
	topic := c.Topic()
	if topic == nil {
		return nil
	}

	log.Info().Str("name", topic.Name).Msg("Topic created")
	//repo, err := svc.ensureRepo(c.Chat(), topic.ThreadID)
	//if err != nil {
	//	log.Error().Err(err).Msg("failed to create repo for topic")
	//	return nil
	//}

	//_, err := svc.Bot.Send(c.Chat(),
	//	fmt.Sprintf("Repo initialized for this topic: `%s`", repo.Path),
	//	&tb.SendOptions{ThreadID: topic.ThreadID, ParseMode: tb.ModeMarkdown})
	//return err
	return nil
}

func (svc *TelegramService) ensureRepo(chat *tb.Chat, threadID int) (*GitRepo, error) {
	if chat == nil {
		return nil, errors.New("missing chat")
	}

	if ctx := svc.getTopicContext(chat.ID, threadID); ctx != nil {
		if strings.TrimSpace(ctx.RepoPath) != "" || strings.TrimSpace(ctx.RepoURL) != "" {
			token := ""
			if svc.git != nil {
				token = svc.git.GitHubToken()
			}
			return svc.ensureRepoFrom(chat, threadID, ctx.RepoURL, ctx.RepoPath, token)
		}
	}

	if svc.git == nil {
		return nil, errors.New("git service not available")
	}

	return svc.git.EnsureTopicRepo(chat.ID, threadID)
}

func (svc *TelegramService) ensureRepoFrom(chat *tb.Chat, threadID int, repoURL, repoPath, token string) (*GitRepo, error) {
	if svc.git == nil {
		log.Error().Msg("ensure repo from: git service not available")
		return nil, errors.New("git service not available")
	}

	if chat == nil {
		log.Error().Msg("ensure repo from: missing chat")
		return nil, errors.New("missing chat")
	}

	logger := log.With().
		Int64("chat", chat.ID).
		Int("topic", threadID).
		Str("repo_url", repoURL).
		Str("repo_path", repoPath).
		Bool("has_token", strings.TrimSpace(token) != "").
		Logger()

	if strings.TrimSpace(repoPath) != "" {
		logger.Info().Msg("ensure repo from path")
		repo, err := svc.git.EnsureTopicRepoFromPath(chat.ID, threadID, repoPath)
		if err != nil {
			logger.Error().Err(err).Msg("failed to ensure repo from path")
		}
		return repo, err
	}

	if repoURL == "" {
		logger.Info().Msg("ensure repo default")
		repo, err := svc.git.EnsureTopicRepo(chat.ID, threadID)
		if err != nil {
			logger.Error().Err(err).Msg("failed to ensure default repo")
		}
		return repo, err
	}

	logger.Info().Msg("ensure repo from url")
	repo, err := svc.git.EnsureTopicRepoFrom(chat.ID, threadID, repoURL, token)
	if err != nil {
		logger.Error().Err(err).Msg("failed to ensure repo from url")
	}
	return repo, err
}

func (svc *TelegramService) getTopicContext(chatID int64, threadID int) *TopicContext {
	key := topicKey(chatID, threadID)
	svc.mu.Lock()
	ctx := svc.topicContexts[key]
	svc.mu.Unlock()
	return ctx
}

func (svc *TelegramService) setTopicContext(chatID int64, threadID int, ctx *TopicContext) {
	key := topicKey(chatID, threadID)
	svc.mu.Lock()
	svc.topicContexts[key] = ctx
	svc.mu.Unlock()
	if err := svc.saveTopicContexts(); err != nil {
		log.Error().Err(err).Msg("failed to save topic contexts")
	}
}

func (svc *TelegramService) deleteTopicContext(chatID int64, threadID int) {
	key := topicKey(chatID, threadID)
	svc.mu.Lock()
	delete(svc.topicContexts, key)
	svc.mu.Unlock()
	if err := svc.saveTopicContexts(); err != nil {
		log.Error().Err(err).Msg("failed to save topic contexts")
	}
}

func (svc *TelegramService) loadTopicContexts() error {
	if svc.topicContextsPath == "" {
		return nil
	}
	data, err := os.ReadFile(svc.topicContextsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var ctxs map[string]*TopicContext
	if err := json.Unmarshal(data, &ctxs); err != nil {
		return err
	}
	if ctxs == nil {
		ctxs = make(map[string]*TopicContext)
	}

	svc.mu.Lock()
	svc.topicContexts = ctxs
	svc.mu.Unlock()

	return nil
}

func (svc *TelegramService) saveTopicContexts() error {
	if svc.topicContextsPath == "" {
		return nil
	}

	snapshot := make(map[string]*TopicContext)
	svc.mu.Lock()
	for key, ctx := range svc.topicContexts {
		if ctx == nil {
			continue
		}
		copyCtx := *ctx
		snapshot[key] = &copyCtx
	}
	svc.mu.Unlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(svc.topicContextsPath)
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(dir, "telegram_topics_*.json")
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(tmpFile.Name())
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	return os.Rename(tmpFile.Name(), svc.topicContextsPath)
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

func (svc *TelegramService) createWorkingBranch(repo *GitRepo, branch string) (string, error) {
	if svc.git == nil {
		return "", errors.New("git service not available")
	}

	return svc.git.CreateWorkingBranch(repo, branch)
}

func (svc *TelegramService) commitAndOpenPR(repo *GitRepo, message string) (*CommitPRResult, error) {
	if svc.git == nil {
		return nil, errors.New("git service not available")
	}

	return svc.git.CommitPushAndOpenPR(repo, message)
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
	return false
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

func (svc *TelegramService) findTopicForRepo(repoURL, repoPath string) (int, bool) {
	urlKey := normalizeRepoURL(repoURL)
	pathKey := normalizeRepoPath(repoPath)
	if urlKey == "" && pathKey == "" {
		return 0, false
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()

	for key, ctx := range svc.topicContexts {
		if ctx == nil {
			continue
		}
		if urlKey != "" && normalizeRepoURL(ctx.RepoURL) == urlKey {
			if _, threadID, ok := parseTopicKey(key); ok {
				return threadID, true
			}
		}
		if pathKey != "" && normalizeRepoPath(ctx.RepoPath) == pathKey {
			if _, threadID, ok := parseTopicKey(key); ok {
				return threadID, true
			}
		}
	}

	return 0, false
}

func normalizeRepoURL(repoURL string) string {
	trimmed := strings.TrimSpace(repoURL)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimSuffix(trimmed, "/")
	trimmed = strings.TrimSuffix(trimmed, ".git")

	if strings.HasPrefix(trimmed, "git@") {
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) == 2 {
			host := strings.TrimPrefix(parts[0], "git@")
			path := strings.TrimPrefix(parts[1], "/")
			return strings.ToLower(host + "/" + strings.TrimSuffix(path, ".git"))
		}
	}

	if strings.HasPrefix(trimmed, "ssh://") {
		parsed, err := url.Parse(trimmed)
		if err == nil && parsed.Host != "" {
			path := strings.TrimPrefix(parsed.Path, "/")
			return strings.ToLower(parsed.Host + "/" + strings.TrimSuffix(path, ".git"))
		}
	}

	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err == nil && parsed.Host != "" {
			path := strings.TrimPrefix(parsed.Path, "/")
			return strings.ToLower(parsed.Host + "/" + strings.TrimSuffix(path, ".git"))
		}
	}

	return strings.ToLower(trimmed)
}

func normalizeRepoPath(repoPath string) string {
	trimmed := strings.TrimSpace(repoPath)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			trimmed = filepath.Join(home, strings.TrimPrefix(trimmed, "~"))
		}
	}
	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		return filepath.Clean(trimmed)
	}
	return filepath.Clean(absPath)
}

func parseTopicKey(key string) (int64, int, bool) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	chatID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	threadID, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return chatID, threadID, true
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

func (svc *TelegramService) onPreview(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		return nil
	}
	if !msg.TopicMessage || msg.ThreadID == 0 {
		return c.Send("Use /preview inside a topic.")
	}
	if svc.preview == nil {
		return c.Send("Preview service not available.")
	}

	payload := strings.TrimSpace(msg.Payload)
	fields := strings.Fields(payload)
	action := "start"
	tunnel := ""

	if len(fields) > 0 {
		switch strings.ToLower(fields[0]) {
		case "start", "status", "stop":
			action = strings.ToLower(fields[0])
			if len(fields) > 1 {
				tunnel = strings.ToLower(fields[1])
			}
		case "ngrok", "tailscale":
			tunnel = strings.ToLower(fields[0])
		default:
			return c.Send("Usage: /preview [start|status|stop] [ngrok|tailscale]")
		}
	}

	switch action {
	case "status":
		if session, ok := svc.preview.PreviewStatus(c.Chat().ID, msg.ThreadID); ok {
			return c.Send(fmt.Sprintf("Preview running:\nURL: %s\nTunnel: %s\nPort: %d", session.URL, session.Tunnel, session.Port),
				&tb.SendOptions{ThreadID: msg.ThreadID})
		}
		return c.Send("No preview running for this topic.", &tb.SendOptions{ThreadID: msg.ThreadID})
	case "stop":
		if err := svc.preview.StopPreview(c.Chat().ID, msg.ThreadID); err != nil {
			log.Error().Err(err).Msg("failed to stop preview")
			return c.Send("Failed to stop preview.", &tb.SendOptions{ThreadID: msg.ThreadID})
		}
		return c.Send("Preview stopped.", &tb.SendOptions{ThreadID: msg.ThreadID})
	default:
		repo, err := svc.ensureRepo(c.Chat(), msg.ThreadID)
		if err != nil {
			log.Error().Err(err).Msg("failed to ensure repo for preview")
			return c.Send("Couldn't prepare the repo for preview.", &tb.SendOptions{ThreadID: msg.ThreadID})
		}
		session, err := svc.preview.StartPreview(c.Chat().ID, msg.ThreadID, repo.Path, tunnel)
		if err != nil {
			log.Error().Err(err).Msg("failed to start preview")
			return c.Send(fmt.Sprintf("Failed to start preview: %s", err.Error()), &tb.SendOptions{ThreadID: msg.ThreadID})
		}
		msgText := fmt.Sprintf("Preview ready:\nURL: %s\nTunnel: %s\nPort: %d", session.URL, session.Tunnel, session.Port)
		return c.Send(msgText, &tb.SendOptions{ThreadID: msg.ThreadID})
	}
}

func (svc *TelegramService) onBranch(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		return nil
	}
	if !msg.TopicMessage || msg.ThreadID == 0 {
		return c.Send("Use /branch inside a topic.")
	}

	branch := strings.TrimSpace(msg.Payload)
	if branch == "" {
		return c.Send("Usage: /branch <name>", &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	repo, err := svc.ensureRepo(c.Chat(), msg.ThreadID)
	if err != nil {
		log.Error().Err(err).Msg("failed to ensure repo for branch")
		return c.Send("Couldn't prepare the repo for this topic.", &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	selectedBranch, err := svc.createWorkingBranch(repo, branch)
	if err != nil {
		log.Error().Err(err).Str("branch", branch).Msg("failed to create working branch")
		return c.Send(fmt.Sprintf("Failed to create branch: %s", err.Error()), &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	return c.Send(fmt.Sprintf("Checked out branch %s.", selectedBranch), &tb.SendOptions{ThreadID: msg.ThreadID})
}

func (svc *TelegramService) onPull(c tb.Context) error {
	msg := c.Message()
	if msg == nil || !msg.TopicMessage || msg.ThreadID == 0 {
		return c.Send("Use /pull inside a topic.")
	}

	repo, err := svc.ensureRepo(c.Chat(), msg.ThreadID)
	if err != nil {
		log.Error().Err(err).Msg("failed to ensure repo for pull")
		return c.Send("Couldn't prepare the repo for this topic.", &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	if err := svc.git.PullMain(repo); err != nil {
		log.Error().Err(err).Str("repo_path", repo.Path).Msg("failed to pull main")
		return c.Send(fmt.Sprintf("Failed to pull main: %s", err.Error()), &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	return c.Send("Pulled latest changes on main.", &tb.SendOptions{ThreadID: msg.ThreadID})
}

func (svc *TelegramService) onCommit(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		return nil
	}
	if !msg.TopicMessage || msg.ThreadID == 0 {
		return c.Send("Use /commit inside a topic.")
	}

	repo, err := svc.ensureRepo(c.Chat(), msg.ThreadID)
	if err != nil {
		log.Error().Err(err).Msg("failed to ensure repo for commit")
		return c.Send("Couldn't prepare the repo for this topic.", &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	result, err := svc.commitAndOpenPR(repo, msg.Payload)
	if err != nil {
		log.Error().Err(err).Msg("failed to commit and open pr")
		return c.Send(fmt.Sprintf("Commit flow failed: %s", err.Error()), &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	resp := fmt.Sprintf("Committed and pushed to %s\nMessage: %s\nPR: %s", result.Branch, result.CommitMessage, result.PRURL)
	return c.Send(resp, &tb.SendOptions{ThreadID: msg.ThreadID})
}

func (svc *TelegramService) onRestart(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		return nil
	}

	opts := &tb.SendOptions{}
	if msg.TopicMessage && msg.ThreadID != 0 {
		opts.ThreadID = msg.ThreadID
	}
	if err := c.Send("Restarting gocode...", opts); err != nil {
		return err
	}

	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := svc.restartProcess(); err != nil {
			log.Error().Err(err).Msg("failed to restart process")
		}
	}()

	return nil
}

func (svc *TelegramService) restartProcess() error {
	projectDir, err := svc.resolveProjectDir()
	if err != nil {
		return err
	}

	restartCommands := [][]string{
		{"go", "mod", "tidy"},
		{"go", "mod", "vendor"},
		{"go", "build", "./runtime/gocode.go"},
	}
	for _, args := range restartCommands {
		if err := svc.runRestartCommand(projectDir, args...); err != nil {
			return err
		}
	}

	executablePath, err := os.Executable()
	if err != nil {
		return err
	}

	cmd := exec.Command(executablePath, os.Args[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = projectDir

	if err := cmd.Start(); err != nil {
		return err
	}

	log.Info().Int("new_pid", cmd.Process.Pid).Msg("spawned replacement gocode process")
	return syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

func (svc *TelegramService) resolveProjectDir() (string, error) {
	if wd, err := os.Getwd(); err == nil {
		if _, statErr := os.Stat(filepath.Join(wd, "go.mod")); statErr == nil {
			return wd, nil
		}
	}

	executablePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	executableDir := filepath.Dir(executablePath)
	if _, statErr := os.Stat(filepath.Join(executableDir, "go.mod")); statErr == nil {
		return executableDir, nil
	}

	return "", errors.New("could not determine project root containing go.mod")
}

func (svc *TelegramService) runRestartCommand(projectDir string, args ...string) error {
	if len(args) == 0 {
		return errors.New("restart command was empty")
	}

	log.Info().Str("command", strings.Join(args, " ")).Msg("running restart command")
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = projectDir
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		out := strings.TrimSpace(string(output))
		if out == "" {
			return fmt.Errorf("restart command failed: %s: %w", strings.Join(args, " "), err)
		}
		return fmt.Errorf("restart command failed: %s: %w: %s", strings.Join(args, " "), err, out)
	}

	return nil
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
