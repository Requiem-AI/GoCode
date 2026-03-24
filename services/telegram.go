package services

import (
	ctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

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
	port              int
	pingServer        *http.Server
	runQueueMu        sync.Mutex
	runQueues         map[string]chan func()
	outboundQueue     chan *telegramOutboundTask
	outboundStop      chan struct{}
	outboundWG        sync.WaitGroup

	deleteTopicMarkup  *tb.ReplyMarkup
	deleteTopicConfirm tb.Btn
	deleteTopicCancel  tb.Btn
}

type TopicContext struct {
	Messages []string
	RepoURL  string
	RepoPath string
}

type detectedFileURI struct {
	Raw  string
	Path string
}

type telegramOutboundTask struct {
	kind        string
	attempt     int
	maxAttempts int
	backoff     time.Duration
	run         func() error
	done        chan error
	doneOnce    sync.Once
}

const TELEGRAM_SVC = "telegram_svc"

var fileURIPattern = regexp.MustCompile(`file://[^\s<>"'` + "`" + `]+`)

// safeWebhookPoller mirrors telebot's webhook poller behavior but avoids
// closing the shared stop channel, which can panic on shutdown.
type safeWebhookPoller struct {
	webhook *tb.Webhook
}

func (p *safeWebhookPoller) Poll(b *tb.Bot, dest chan tb.Update, stop chan struct{}) {
	if err := b.SetWebhook(p.webhook); err != nil {
		b.OnError(err, nil)
		close(stop)
		return
	}

	if p.webhook.Listen == "" {
		<-stop
		return
	}

	server := &http.Server{
		Addr: p.webhook.Listen,
		Handler: &safeWebhookHandler{
			secretToken: p.webhook.SecretToken,
			dest:        dest,
		},
	}

	go func() {
		<-stop
		if err := server.Shutdown(ctx.Background()); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.OnError(err, nil)
		}
	}()

	var err error
	if p.webhook.TLS != nil {
		err = server.ListenAndServeTLS(p.webhook.TLS.Cert, p.webhook.TLS.Key)
	} else {
		err = server.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		b.OnError(err, nil)
	}
}

type safeWebhookHandler struct {
	secretToken string
	dest        chan<- tb.Update
}

func (h *safeWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.secretToken != "" && r.Header.Get("X-Telegram-Bot-Api-Secret-Token") != h.secretToken {
		return
	}

	var update tb.Update
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		return
	}
	h.dest <- update
}

func (svc TelegramService) Id() string {
	return TELEGRAM_SVC
}

func (svc *TelegramService) Configure(ctx *context.Context) (err error) {
	allowedUserID, err := svc.parseAllowedUserID()
	if err != nil {
		return err
	}
	svc.allowedUserID = allowedUserID

	port, err := strconv.Atoi(os.Getenv("TELEGRAM_PORT"))
	if err != nil {
		return fmt.Errorf("invalid TELEGRAM_PORT %w", err)
	}
	svc.port = port
	if svc.port == 0 {
		return fmt.Errorf("TELEGRAM_PORT is required for webhook mode")
	}

	webhook := &tb.Webhook{
		Listen: fmt.Sprintf(":%d", svc.port),
		Endpoint: &tb.WebhookEndpoint{
			PublicURL: os.Getenv("TELEGRAM_WEBHOOK"),
		},
	}

	svc.Bot, err = tb.NewBot(tb.Settings{
		Token:  os.Getenv("TELEGRAM_SECRET"),
		Poller: &safeWebhookPoller{webhook: webhook},
		Client: &http.Client{
			Timeout: 60 * time.Second,
		},
		OnError: func(err error, c tb.Context) {
			svc.decorateTelegramEvent(log.Error().Err(err), c).Msg("telegram bot error")
		},
	})
	if err != nil {
		return err
	}

	svc.topicContexts = make(map[string]*TopicContext)
	svc.runQueues = make(map[string]chan func())
	svc.outboundQueue = make(chan *telegramOutboundTask, 256)
	svc.outboundStop = make(chan struct{})
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
	log.Info().Msg("telegram service starting")
	svc.agent = svc.Service(Agent_SVC).(*AgentService)
	svc.git = svc.Service(GIT_SVC).(*GitService)
	svc.preview = svc.Service(PREVIEW_SVC).(*PreviewService)

	if err := svc.loadTopicContexts(); err != nil {
		log.Error().Err(err).Msg("failed to load topic contexts")
	}

	svc.startOutboundWorkers()
	svc.setupHandlers()
	svc.setupEvents()
	svc.sendOnlineMessage()
	svc.startPingServer()

	log.Info().Int("port", svc.port).Msg("telegram bot webhook started")
	svc.Bot.Start()

	log.Info().Msg("telegram bot webhook stopped")
	return nil
}

func (svc *TelegramService) Shutdown() {
	log.Info().Msg("telegram service shutting down")
	svc.stopOutboundWorkers()
	if svc.pingServer != nil {
		if err := svc.pingServer.Close(); err != nil {
			log.Error().Err(err).Msg("ping server shutdown error")
		}
	}
	if svc.Bot == nil {
		log.Warn().Msg("telegram shutdown: bot is nil")
		return
	}
	svc.Bot.Stop()
	log.Info().Msg("telegram service stopped")
}

func (svc *TelegramService) setupHandlers() {
	svc.Bot.Handle("/clear", svc.guardHandler(svc.onClear))
	svc.Bot.Handle("/new", svc.guardHandler(svc.onTopic))
	svc.Bot.Handle("/delete", svc.guardHandler(svc.onDeleteTopic))
	svc.Bot.Handle("/github", svc.guardHandler(svc.onGithub))
	svc.Bot.Handle("/git", svc.guardHandler(svc.onGit))
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

	_, err := svc.sendWithRetry(&tb.Chat{ID: chatID}, message, nil)
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
			// Keep the bot running on transient Telegram API/network failures.
			// Errors are logged above for diagnosis.
			return nil
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
		if parsed, _, ok := parseCommandText(text); ok {
			command = strings.TrimPrefix(parsed, "/")
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

func (svc *TelegramService) startPingServer() {
	if svc.port == 0 {
		log.Warn().Msg("TELEGRAM_PORT not set, skipping ping server")
		return
	}

	pingPort := svc.port + 1
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	})

	svc.pingServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", pingPort),
		Handler: mux,
	}

	go func() {
		log.Info().Int("port", pingPort).Msg("ping server starting")
		if err := svc.pingServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("ping server error")
		}
	}()
}

func (svc *TelegramService) startOutboundWorkers() {
	const workerCount = 4
	for i := 0; i < workerCount; i++ {
		svc.outboundWG.Add(1)
		go svc.processOutboundQueue(i + 1)
	}
}

func (svc *TelegramService) stopOutboundWorkers() {
	select {
	case <-svc.outboundStop:
		return
	default:
		close(svc.outboundStop)
	}

	for {
		select {
		case task := <-svc.outboundQueue:
			svc.completeOutboundTask(task, errors.New("telegram service shutting down"))
		default:
			svc.outboundWG.Wait()
			return
		}
	}
}

func (svc *TelegramService) processOutboundQueue(workerID int) {
	defer svc.outboundWG.Done()
	logger := log.With().Int("worker", workerID).Logger()

	for {
		select {
		case <-svc.outboundStop:
			return
		case task := <-svc.outboundQueue:
			if task == nil || task.run == nil {
				continue
			}

			task.attempt++
			err := task.run()
			if err == nil {
				svc.completeOutboundTask(task, nil)
				continue
			}

			retryable := isRetryableTelegramSendError(err)
			if retryable && task.attempt < task.maxAttempts {
				delay := task.backoff
				if delay <= 0 {
					delay = 250 * time.Millisecond
				}
				task.backoff = delay * 2
				logger.Warn().
					Err(err).
					Str("kind", task.kind).
					Int("attempt", task.attempt).
					Dur("retry_in", delay).
					Msg("telegram outbound task failed; requeueing")
				svc.scheduleOutboundRequeue(task, delay)
				continue
			}

			logger.Warn().
				Err(err).
				Str("kind", task.kind).
				Int("attempt", task.attempt).
				Bool("retryable", retryable).
				Msg("telegram outbound task failed; giving up")
			svc.completeOutboundTask(task, err)
		}
	}
}

func (svc *TelegramService) scheduleOutboundRequeue(task *telegramOutboundTask, delay time.Duration) {
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-svc.outboundStop:
			svc.completeOutboundTask(task, errors.New("telegram service shutting down"))
			return
		case <-timer.C:
		}

		if err := svc.enqueueOutboundTask(task); err != nil {
			svc.completeOutboundTask(task, err)
		}
	}()
}

func (svc *TelegramService) enqueueOutboundTask(task *telegramOutboundTask) error {
	if task == nil || task.run == nil {
		return errors.New("invalid telegram outbound task")
	}

	select {
	case <-svc.outboundStop:
		return errors.New("telegram outbound queue stopped")
	default:
	}

	select {
	case svc.outboundQueue <- task:
		return nil
	default:
		return errors.New("telegram outbound queue full")
	}
}

func (svc *TelegramService) completeOutboundTask(task *telegramOutboundTask, err error) {
	if task == nil || task.done == nil {
		return
	}
	task.doneOnce.Do(func() {
		task.done <- err
		close(task.done)
	})
}

func (svc *TelegramService) onText(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		log.Warn().Msg("onText: nil message, ignoring")
		return nil
	}

	if strings.HasPrefix(msg.Text, "/") {
		handled, err := svc.dispatchTextCommand(c)
		if handled {
			return err
		}
		log.Debug().Str("text", msg.Text).Msg("onText: unrecognized command, ignoring")
		return nil
	}

	// Capture everything we need from the telebot context — the context must
	// not be used after the handler returns because telebot may recycle it.
	chat := c.Chat()
	text := c.Text()
	threadID := 0
	msgRef := c.Message()
	opts := &tb.SendOptions{}

	if msg.TopicMessage && msg.ThreadID != 0 {
		threadID = msg.ThreadID
		opts.ThreadID = threadID
		log.Info().Str("text", text).Int("topic", threadID).Msg("onText topic")
	} else {
		log.Info().Str("text", text).Msg("onText main chat")
	}

	// Enqueue all blocking work (react, ensureRepo, agent run) so the
	// telebot polling loop is never held up.
	svc.enqueueWork(chat, threadID, func() {
		logger := log.With().Int64("chat_id", chat.ID).Int("thread_id", threadID).Logger()

		if err := svc.reactWithRetry(chat, msgRef, tb.ReactionOptions{Reactions: []tb.Reaction{{
			Type:  "emoji",
			Emoji: "👍",
		}}}); err != nil {
			logger.Warn().Err(err).Msg("onText: failed to react")
		}

		repoPath := ""
		if threadID != 0 {
			repo, err := svc.ensureRepo(chat, threadID)
			if err != nil {
				logger.Error().Err(err).Msg("onText: failed to ensure repo")
				if _, sendErr := svc.sendWithRetry(chat, "Couldn't prepare the repo for this topic.", opts); sendErr != nil {
					logger.Warn().Err(sendErr).Msg("onText: failed to send repo error")
				}
				return
			}
			repoPath = repo.Path
		}

		svc.runAgentWithPendingUpdates(chat, opts, repoPath, text)
	})
	return nil
}

func (svc *TelegramService) dispatchTextCommand(c tb.Context) (bool, error) {
	msg := c.Message()
	if msg == nil {
		log.Warn().Msg("dispatchTextCommand: nil message")
		return false, nil
	}

	command, payload, ok := parseCommandText(msg.Text)
	if !ok {
		return false, nil
	}

	msg.Payload = payload

	switch command {
	case "/clear":
		return true, svc.onClear(c)
	case "/new":
		return true, svc.onTopic(c)
	case "/delete":
		return true, svc.onDeleteTopic(c)
	case "/github":
		return true, svc.onGithub(c)
	case "/git":
		return true, svc.onGit(c)
	case "/pull":
		return true, svc.onPull(c)
	case "/preview":
		return true, svc.onPreview(c)
	case "/branch":
		return true, svc.onBranch(c)
	case "/commit":
		return true, svc.onCommit(c)
	case "/restart":
		return true, svc.onRestart(c)
	default:
		return false, nil
	}
}

func parseCommandText(text string) (command string, payload string, ok bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", "", false
	}

	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", "", false
	}

	commandToken := fields[0]
	if atIdx := strings.Index(commandToken, "@"); atIdx >= 0 {
		commandToken = commandToken[:atIdx]
	}
	if commandToken == "" || !strings.HasPrefix(commandToken, "/") {
		return "", "", false
	}

	payload = strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
	return commandToken, payload, true
}

func (svc *TelegramService) runAgentWithPendingUpdates(chat *tb.Chat, opts *tb.SendOptions, repoPath, prompt string) {
	if opts == nil {
		opts = &tb.SendOptions{}
	}

	logger := log.With().
		Int64("chat_id", chat.ID).
		Int("thread_id", opts.ThreadID).
		Str("repo", repoPath).
		Logger()

	logger.Info().Msg("runAgentWithPendingUpdates: starting")

	if opts.ThreadID != 0 {
		if err := svc.Bot.Notify(chat, tb.Typing, opts.ThreadID); err != nil {
			logger.Warn().Err(err).Msg("failed to send typing indicator")
		}
	} else {
		if err := svc.Bot.Notify(chat, tb.Typing); err != nil {
			logger.Warn().Err(err).Msg("failed to send typing indicator")
		}
	}

	started := time.Now()
	var pendingMessageID atomic.Int64
	pendingOpts := cloneSendOptions(opts)
	pendingOpts.DisableNotification = true

	stopUpdates := make(chan struct{})
	updatesDone := make(chan struct{})
	go func() {
		defer close(updatesDone)

		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-stopUpdates:
				logger.Debug().Msg("pending updates loop stopped")
				return
			case <-ticker.C:
				elapsed := int(time.Since(started).Seconds())
				updateText := fmt.Sprintf("Still thinking... (%ds elapsed)", elapsed)
				currentPendingMessageID := int(pendingMessageID.Load())
				nextMessageID, updateErr := svc.editOrSendByMessageID(chat, pendingOpts, currentPendingMessageID, updateText, "")
				if updateErr != nil {
					logger.Warn().Err(updateErr).Msg("failed to update pending message")
					continue
				}
				pendingMessageID.Store(int64(nextMessageID))
			}
		}
	}()

	logger.Info().Msg("calling agent.RunWithEvents")
	resp, runErr := svc.agent.RunWithEvents(repoPath, prompt, func(event AgentEvent) {
		evtText := formatAgentEventMessage(event)
		if strings.TrimSpace(evtText) == "" {
			return
		}
		if _, err := svc.sendWithRetry(chat, evtText, opts); err != nil {
			logger.Warn().Err(err).Msg("failed to send intra-agent event message")
		}
	})
	elapsed := time.Since(started)
	close(stopUpdates)
	select {
	case <-updatesDone:
	case <-time.After(2 * time.Second):
		logger.Warn().Msg("timed out waiting for pending updates loop to stop")
	}

	if runErr != nil {
		logger.Error().Err(runErr).Dur("elapsed", elapsed).Msg("agent.Run failed")
		failureText := formatAgentFailureResponse(runErr, resp)
		if err := svc.sendFinalResponse(chat, opts, int(pendingMessageID.Load()), failureText, ""); err != nil {
			logger.Warn().Err(err).Msg("failed to send agent failure response")
		}
		return
	}

	logger.Info().Dur("elapsed", elapsed).Int("response_len", len(resp)).Msg("agent.Run completed")

	fileURIs := detectFileURIs(resp)
	responseText := resp
	if len(fileURIs) > 0 {
		responseText = stripDetectedFileURIs(responseText, fileURIs)
	}
	escaped := escapeMarkdownV2(responseText)
	if len(fileURIs) > 0 {
		resolvedPath, usedIdx, resolveErr := resolveFirstDetectedFilePath(repoPath, fileURIs)
		if resolveErr == nil && utf8.RuneCountInString(resp) <= telegramMaxCaptionLength {
			sendErr := svc.sendFinalResponseDocument(chat, opts, int(pendingMessageID.Load()), escaped, tb.ModeMarkdownV2, resolvedPath)
			if sendErr != nil {
				logger.Warn().Err(sendErr).Msg("markdownV2 document send failed, retrying as plain text")
				sendErr = svc.sendFinalResponseDocument(chat, opts, int(pendingMessageID.Load()), responseText, "", resolvedPath)
			}
			if sendErr == nil {
				remaining := removeDetectedFileByIndex(fileURIs, usedIdx)
				if err := svc.sendDetectedFiles(chat, opts, repoPath, remaining); err != nil {
					logger.Warn().Err(err).Int("count", len(remaining)).Msg("failed to send one or more additional file attachments")
				}
				return
			}
			logger.Warn().Err(sendErr).Msg("failed to send final agent response with attachment, falling back to text response")
		}
	}

	sendErr := svc.sendFinalResponse(chat, opts, int(pendingMessageID.Load()), escaped, tb.ModeMarkdownV2)
	if sendErr != nil {
		logger.Warn().Err(sendErr).Msg("markdownV2 send failed, retrying as plain text")
		sendErr = svc.sendFinalResponse(chat, opts, int(pendingMessageID.Load()), responseText, "")
	}
	if sendErr != nil {
		logger.Error().Err(sendErr).Msg("failed to send final agent response")
		return
	}

	if len(fileURIs) == 0 {
		return
	}
	if err := svc.sendDetectedFiles(chat, opts, repoPath, fileURIs); err != nil {
		logger.Warn().Err(err).Int("count", len(fileURIs)).Msg("failed to send one or more file attachments")
	}
}

const maxAgentFailureDetailsLen = 3000

func formatAgentEventMessage(event AgentEvent) string {
	body := strings.TrimSpace(event.Text)
	if body == "" {
		return ""
	}

	switch event.Type {
	case AgentEventForward:
		return fmt.Sprintf("@%s %s", event.To, body)
	case AgentEventResponse:
		return fmt.Sprintf("@%s %s", event.To, body)
	default:
		return ""
	}
}

func formatAgentFailureResponse(runErr error, output string) string {
	lines := []string{"Agent failed to run."}
	if runErr != nil {
		lines = append(lines, "Error: "+strings.TrimSpace(runErr.Error()))
	}

	cleaned := strings.TrimSpace(output)
	if cleaned == "" {
		return strings.Join(lines, "\n")
	}

	if runErr != nil && cleaned == strings.TrimSpace(runErr.Error()) {
		return strings.Join(lines, "\n")
	}

	if len(cleaned) > maxAgentFailureDetailsLen {
		cleaned = cleaned[:maxAgentFailureDetailsLen] + "\n...[truncated]"
	}

	lines = append(lines, "", "Agent output:", cleaned)
	return strings.Join(lines, "\n")
}

func detectFileURIs(text string) []detectedFileURI {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	matches := fileURIPattern.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(matches))
	files := make([]detectedFileURI, 0, len(matches))
	for _, match := range matches {
		candidate := trimFileURI(match)
		if candidate == "" {
			continue
		}
		decoded, err := parseFileURIPath(candidate)
		if err != nil || decoded == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		files = append(files, detectedFileURI{
			Raw:  candidate,
			Path: decoded,
		})
	}

	return files
}

func stripDetectedFileURIs(text string, files []detectedFileURI) string {
	if strings.TrimSpace(text) == "" || len(files) == 0 {
		return text
	}

	cleaned := text
	for _, file := range files {
		raw := strings.TrimSpace(file.Raw)
		if raw == "" {
			continue
		}

		// Convert markdown links like [artifact](file://path) to plain "artifact".
		linkPattern := regexp.MustCompile(`\[([^\]]+)\]\(` + regexp.QuoteMeta(raw) + `\)`)
		cleaned = linkPattern.ReplaceAllString(cleaned, `$1`)
		cleaned = strings.ReplaceAll(cleaned, raw, "")
	}

	return strings.TrimSpace(cleaned)
}

func trimFileURI(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	const trailing = ".,;:!?)]}>"
	return strings.TrimRight(trimmed, trailing)
}

func parseFileURIPath(raw string) (string, error) {
	if !strings.HasPrefix(raw, "file://") {
		return "", fmt.Errorf("not a file uri")
	}

	withoutScheme := strings.TrimPrefix(raw, "file://")
	if withoutScheme == "" {
		return "", fmt.Errorf("missing file path")
	}

	decoded, err := url.PathUnescape(withoutScheme)
	if err != nil {
		return "", err
	}

	return decoded, nil
}

func resolveFilePath(repoPath, candidatePath string) (string, error) {
	candidatePath = strings.TrimSpace(candidatePath)
	if candidatePath == "" {
		return "", fmt.Errorf("empty file path")
	}

	var resolved string
	switch {
	case filepath.IsAbs(candidatePath):
		resolved = filepath.Clean(candidatePath)
	case repoPath != "":
		resolved = filepath.Join(repoPath, candidatePath)
	default:
		absPath, err := filepath.Abs(candidatePath)
		if err != nil {
			return "", err
		}
		resolved = absPath
	}

	if repoPath == "" {
		return resolved, nil
	}

	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(repoAbs, targetAbs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("outside topic repo")
	}

	return targetAbs, nil
}

func resolveFirstDetectedFilePath(repoPath string, files []detectedFileURI) (string, int, error) {
	for i, file := range files {
		resolvedPath, err := resolveFilePath(repoPath, file.Path)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolvedPath)
		if err != nil || info.IsDir() {
			continue
		}
		return resolvedPath, i, nil
	}
	return "", -1, errors.New("no sendable detected files")
}

func removeDetectedFileByIndex(files []detectedFileURI, idx int) []detectedFileURI {
	if idx < 0 || idx >= len(files) {
		return files
	}
	remaining := make([]detectedFileURI, 0, len(files)-1)
	remaining = append(remaining, files[:idx]...)
	remaining = append(remaining, files[idx+1:]...)
	return remaining
}

func (svc *TelegramService) sendDetectedFiles(chat *tb.Chat, baseOpts *tb.SendOptions, repoPath string, files []detectedFileURI) error {
	if len(files) == 0 {
		return nil
	}

	opts := cloneSendOptions(baseOpts)
	opts.ParseMode = ""
	opts.DisableNotification = false

	var failures []string
	for _, file := range files {
		resolvedPath, err := resolveFilePath(repoPath, file.Path)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s (%s)", file.Raw, err.Error()))
			continue
		}

		info, err := os.Stat(resolvedPath)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s (%s)", file.Raw, err.Error()))
			continue
		}
		if info.IsDir() {
			failures = append(failures, fmt.Sprintf("%s (is a directory)", file.Raw))
			continue
		}

		doc := &tb.Document{
			File:     tb.FromDisk(resolvedPath),
			FileName: filepath.Base(resolvedPath),
		}
		if _, err := svc.sendDocumentWithRetry(chat, doc, opts); err != nil {
			failures = append(failures, fmt.Sprintf("%s (%s)", file.Raw, err.Error()))
		}
	}

	if len(failures) == 0 {
		return nil
	}

	errMsg := "Some file attachments could not be sent:\n- " + strings.Join(failures, "\n- ")
	_, sendErr := svc.sendWithRetry(chat, truncateTelegramText(errMsg), opts)
	if sendErr != nil {
		return sendErr
	}
	return errors.New("one or more file attachments failed")
}

// enqueueWork submits a function to run without blocking the telebot handler.
// Work for the same chat+thread is serialized via a per-topic queue.
func (svc *TelegramService) enqueueWork(chat *tb.Chat, threadID int, work func()) {
	key := topicKey(chat.ID, threadID)
	logger := log.With().Str("queue_key", key).Logger()

	svc.runQueueMu.Lock()
	queue, ok := svc.runQueues[key]
	if !ok {
		queue = make(chan func(), 64)
		svc.runQueues[key] = queue
		go svc.processRunQueue(key, queue)
		logger.Info().Msg("created new run queue")
	}
	svc.runQueueMu.Unlock()

	select {
	case queue <- work:
		logger.Debug().Msg("work enqueued")
	default:
		logger.Error().Msg("run queue full, dropping request")
		opts := &tb.SendOptions{}
		if threadID != 0 {
			opts.ThreadID = threadID
		}
		if _, err := svc.sendWithRetry(chat, "Too many pending requests, try again shortly.", opts); err != nil {
			logger.Warn().Err(err).Msg("failed to send queue-full message")
		}
	}
}

func (svc *TelegramService) processRunQueue(key string, queue <-chan func()) {
	logger := log.With().Str("queue_key", key).Logger()
	logger.Info().Msg("run queue processor started")
	for task := range queue {
		logger.Debug().Msg("run queue processing next task")
		svc.safeRunTask(logger, task)
	}
	logger.Info().Msg("run queue processor exited")
}

func (svc *TelegramService) safeRunTask(logger zerolog.Logger, task func()) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error().Interface("panic", r).Msg("recovered panic in run queue task")
		}
	}()
	task()
}

func (svc *TelegramService) sendFinalResponse(chat *tb.Chat, baseOpts *tb.SendOptions, pendingMessageID int, text, parseMode string) error {
	log.Debug().Int("pending_msg_id", pendingMessageID).Int("text_len", len(text)).Str("parse_mode", parseMode).Msg("sendFinalResponse")

	if pendingMessageID != 0 {
		pending := tb.StoredMessage{
			MessageID: strconv.Itoa(pendingMessageID),
			ChatID:    chat.ID,
		}
		if err := svc.Bot.Delete(pending); err != nil {
			log.Warn().Err(err).Int("message_id", pendingMessageID).Msg("failed to delete pending message")
		}
	}

	opts := cloneSendOptions(baseOpts)
	opts.DisableNotification = false
	if parseMode != "" {
		opts.ParseMode = parseMode
	}

	chunks := splitMessage(text, telegramMaxMessageLength)
	log.Debug().Int("chunks", len(chunks)).Msg("sendFinalResponse: split message")

	for i, chunk := range chunks {
		_, err := svc.sendWithRetry(chat, chunk, opts)
		if err != nil {
			log.Warn().Err(err).Str("parse_mode", parseMode).Int("chunk", i+1).Int("total_chunks", len(chunks)).Msg("sendFinalResponse: sendWithRetry failed")
			return err
		}
	}
	return nil
}

const telegramMaxMessageLength = 4096
const telegramMaxCaptionLength = 1024

func (svc *TelegramService) sendFinalResponseDocument(chat *tb.Chat, baseOpts *tb.SendOptions, pendingMessageID int, text, parseMode, filePath string) error {
	log.Debug().Int("pending_msg_id", pendingMessageID).Int("text_len", len(text)).Str("parse_mode", parseMode).Str("file", filePath).Msg("sendFinalResponseDocument")

	if pendingMessageID != 0 {
		pending := tb.StoredMessage{
			MessageID: strconv.Itoa(pendingMessageID),
			ChatID:    chat.ID,
		}
		if err := svc.Bot.Delete(pending); err != nil {
			log.Warn().Err(err).Int("message_id", pendingMessageID).Msg("failed to delete pending message")
		}
	}

	opts := cloneSendOptions(baseOpts)
	opts.DisableNotification = false
	if parseMode != "" {
		opts.ParseMode = parseMode
	}

	doc := &tb.Document{
		File:     tb.FromDisk(filePath),
		FileName: filepath.Base(filePath),
		Caption:  text,
	}
	_, err := svc.sendDocumentWithRetry(chat, doc, opts)
	return err
}

// splitMessage splits text into chunks that fit within maxLen.
// It prefers splitting at newline boundaries. If a single line exceeds maxLen,
// it splits at the last space before the limit, or hard-splits as a last resort.
func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		// Find the best split point within maxLen.
		chunk := text[:maxLen]

		// Try to split at the last newline.
		if idx := strings.LastIndex(chunk, "\n"); idx > 0 {
			chunks = append(chunks, text[:idx])
			text = text[idx+1:] // skip the newline
			continue
		}

		// No newline found; try the last space.
		if idx := strings.LastIndex(chunk, " "); idx > 0 {
			chunks = append(chunks, text[:idx])
			text = text[idx+1:] // skip the space
			continue
		}

		// No good break point; hard split.
		chunks = append(chunks, chunk)
		text = text[maxLen:]
	}

	return chunks
}

func (svc *TelegramService) editOrSendByMessageID(chat *tb.Chat, baseOpts *tb.SendOptions, messageID int, text, parseMode string) (int, error) {
	opts := cloneSendOptions(baseOpts)
	if parseMode != "" {
		opts.ParseMode = parseMode
	}

	if messageID != 0 {
		editable := tb.StoredMessage{
			MessageID: strconv.Itoa(messageID),
			ChatID:    chat.ID,
		}
		editedMsg, err := svc.Bot.Edit(editable, text, opts)
		if err == nil {
			if editedMsg != nil && editedMsg.ID != 0 {
				return editedMsg.ID, nil
			}
			return messageID, nil
		}
		// Once a pending message exists, do not send a new one on edit failure.
		// This prevents duplicate "Still thinking..." messages during transient edit errors.
		return messageID, err
	}

	sentMsg, err := svc.sendWithRetry(chat, text, opts)
	if err != nil {
		return messageID, err
	}
	return sentMsg.ID, nil
}

func cloneSendOptions(opts *tb.SendOptions) *tb.SendOptions {
	if opts == nil {
		return &tb.SendOptions{}
	}
	copy := *opts
	return &copy
}

func (svc *TelegramService) sendWithRetry(chat *tb.Chat, text string, opts *tb.SendOptions) (*tb.Message, error) {
	if opts == nil {
		opts = &tb.SendOptions{}
	}

	var sent *tb.Message
	task := &telegramOutboundTask{
		kind:        "send_message",
		maxAttempts: 3,
		backoff:     250 * time.Millisecond,
		done:        make(chan error, 1),
		run: func() error {
			var err error
			sent, err = svc.Bot.Send(chat, text, opts)
			return err
		},
	}
	if err := svc.enqueueOutboundTask(task); err != nil {
		return nil, err
	}
	if err := <-task.done; err != nil {
		return sent, err
	}
	return sent, nil
}

func (svc *TelegramService) sendDocumentWithRetry(chat *tb.Chat, doc *tb.Document, opts *tb.SendOptions) (*tb.Message, error) {
	if opts == nil {
		opts = &tb.SendOptions{}
	}

	var sent *tb.Message
	task := &telegramOutboundTask{
		kind:        "send_document",
		maxAttempts: 3,
		backoff:     250 * time.Millisecond,
		done:        make(chan error, 1),
		run: func() error {
			var err error
			sent, err = svc.Bot.Send(chat, doc, opts)
			return err
		},
	}
	if err := svc.enqueueOutboundTask(task); err != nil {
		return nil, err
	}
	if err := <-task.done; err != nil {
		return sent, err
	}
	return sent, nil
}

func (svc *TelegramService) reactWithRetry(chat *tb.Chat, msg *tb.Message, opts tb.ReactionOptions) error {
	task := &telegramOutboundTask{
		kind:        "react",
		maxAttempts: 3,
		backoff:     250 * time.Millisecond,
		done:        make(chan error, 1),
		run: func() error {
			return svc.Bot.React(chat, msg, opts)
		},
	}
	if err := svc.enqueueOutboundTask(task); err != nil {
		return err
	}
	return <-task.done
}

func isRetryableTelegramSendError(err error) bool {
	if err == nil {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "operation timed out") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "retry after") ||
		strings.Contains(msg, "internal server error") ||
		strings.Contains(msg, "bad gateway") ||
		strings.Contains(msg, "service unavailable") ||
		strings.Contains(msg, "gateway timeout")
}

func (svc *TelegramService) onClear(c tb.Context) error {
	msg := c.Message()
	if msg == nil || !msg.TopicMessage || msg.ThreadID == 0 {
		log.Debug().Msg("onClear: not in a topic, sending usage hint")
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

	_, err = svc.sendWithRetry(c.Chat(), "Context cleared.", &tb.SendOptions{ThreadID: msg.ThreadID})
	return err
}

func (svc *TelegramService) onDeleteTopic(c tb.Context) error {
	msg := c.Message()
	if msg == nil || !msg.TopicMessage || msg.ThreadID == 0 {
		return c.Send("Use /delete inside the topic you want to remove.")
	}

	_, err := svc.sendWithRetry(c.Chat(),
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
		log.Warn().Msg("onTopic: nil message")
		return nil
	}

	name, repoURL, repoPath := svc.parseTopicArgs(msg.Payload)
	if name == "" {
		return c.Send("Usage: /new <name> [repo-url|repo-path]")
	}

	if repoURL == "" && repoPath == "" {
		if svc.git == nil {
			return c.Send("Git service is not available.")
		}

		createdRepoURL, err := svc.git.CreateGitHubRepo(name)
		if err != nil {
			log.Error().Err(err).Str("name", name).Msg("failed to create github repo for topic")
			return c.Send(fmt.Sprintf("Failed to create GitHub repo: %s", err.Error()))
		}
		repoURL = createdRepoURL
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

	readyMsg := escapeMarkdownV2("Topic ready. Type anything to start")
	_, err = svc.sendWithRetry(c.Chat(),
		readyMsg,
		&tb.SendOptions{ThreadID: topic.ThreadID, ParseMode: tb.ModeMarkdownV2})
	return err
}

func (svc *TelegramService) onTopicCreated(c tb.Context) error {
	topic := c.Topic()
	if topic == nil {
		log.Warn().Msg("onTopicCreated: nil topic")
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
	//	&tb.SendOptions{ThreadID: topic.ThreadID, ParseMode: tb.ModeMarkdownV2})
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

func (svc *TelegramService) commitAndOpenPR(repo *GitRepo, message, prBody string) (*CommitPRResult, error) {
	if svc.git == nil {
		return nil, errors.New("git service not available")
	}

	return svc.git.CommitPushAndOpenPR(repo, message, prBody)
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
		log.Warn().Msg("onGithub: nil message")
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
		log.Warn().Msg("onPreview: nil message")
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
		log.Warn().Msg("onBranch: nil message")
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

func (svc *TelegramService) onGit(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		log.Warn().Msg("onGit: nil message")
		return nil
	}
	if !msg.TopicMessage || msg.ThreadID == 0 {
		return c.Send("Use /git inside a topic.")
	}

	args := strings.Fields(strings.TrimSpace(msg.Payload))
	if len(args) == 0 {
		return c.Send("Usage: /git <args...>\nExample: /git status", &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	repo, err := svc.ensureRepo(c.Chat(), msg.ThreadID)
	if err != nil {
		log.Error().Err(err).Msg("failed to ensure repo for git command")
		return c.Send("Couldn't prepare the repo for this topic.", &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	output, runErr := svc.git.RunTopicGitCommand(repo, args...)
	commandLine := "git " + strings.Join(args, " ")

	if runErr != nil {
		log.Error().
			Err(runErr).
			Str("repo_path", repo.Path).
			Str("git_args", strings.Join(args, " ")).
			Msg("git command failed")

		resp := fmt.Sprintf("Command failed: %s\nError: %s", commandLine, runErr.Error())
		if strings.TrimSpace(output) != "" {
			resp += "\n\n" + truncateTelegramText(output)
		}
		return c.Send(resp, &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	if strings.TrimSpace(output) == "" {
		return c.Send(fmt.Sprintf("Command succeeded: %s\n(no output)", commandLine), &tb.SendOptions{ThreadID: msg.ThreadID})
	}

	return c.Send(truncateTelegramText(output), &tb.SendOptions{ThreadID: msg.ThreadID})
}

func (svc *TelegramService) onCommit(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		log.Warn().Msg("onCommit: nil message")
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

	commitMessage := strings.TrimSpace(msg.Payload)
	if commitMessage == "" {
		generated, genErr := svc.generateCommitMessage(repo)
		if genErr != nil {
			log.Warn().Err(genErr).Str("repo_path", repo.Path).Msg("failed to generate commit message with agent; using fallback")
		} else {
			commitMessage = generated
		}
	}
	prBody := ""
	generatedPRBody, prBodyErr := svc.generatePRDescription(repo, commitMessage)
	if prBodyErr != nil {
		log.Warn().Err(prBodyErr).Str("repo_path", repo.Path).Msg("failed to generate pr description with agent; using fallback")
	} else {
		prBody = generatedPRBody
	}

	pendingID := 0
	pendingOpts := &tb.SendOptions{ThreadID: msg.ThreadID, DisableNotification: true}
	pendingID, err = svc.editOrSendByMessageID(c.Chat(), pendingOpts, pendingID, "Running commit flow...", "")
	if err != nil {
		log.Warn().Err(err).Msg("failed to send commit status message")
		pendingID = 0
	}

	result, err := svc.commitAndOpenPR(repo, commitMessage, prBody)
	if err != nil {
		log.Error().Err(err).Msg("failed to commit and open pr")
		return svc.sendFinalResponse(c.Chat(), &tb.SendOptions{ThreadID: msg.ThreadID}, pendingID, fmt.Sprintf("Commit flow failed: %s", err.Error()), "")
	}

	resp := fmt.Sprintf("Committed and pushed to %s\nMessage: %s\nPR: %s", result.Branch, result.CommitMessage, result.PRURL)
	return svc.sendFinalResponse(c.Chat(), &tb.SendOptions{ThreadID: msg.ThreadID}, pendingID, resp, "")
}

func (svc *TelegramService) generateCommitMessage(repo *GitRepo) (string, error) {
	if repo == nil {
		return "", errors.New("repo is nil")
	}
	if svc.agent == nil {
		return "", errors.New("agent service unavailable")
	}

	const prompt = `Generate a concise Git commit subject for the current repository changes.
Inspect the working tree and staged diff as needed.
Return only the commit subject line.
Requirements:
- imperative mood
- max 72 characters
- no quotes, markdown, bullets, or code fences`

	resp, err := svc.agent.Run(repo.Path, prompt)
	if err != nil {
		return "", err
	}

	commitMessage := sanitizeAgentCommitMessage(resp)
	if commitMessage == "" {
		return "", errors.New("agent returned an empty commit message")
	}

	return commitMessage, nil
}

func (svc *TelegramService) generatePRDescription(repo *GitRepo, commitMessage string) (string, error) {
	if repo == nil {
		return "", errors.New("repo is nil")
	}
	if svc.agent == nil {
		return "", errors.New("agent service unavailable")
	}

	prompt := fmt.Sprintf(`Generate a concise GitHub pull request description for the current repository changes.
Inspect the working tree and staged diff as needed.
Return only the PR description text (plain text, no markdown fences).
Commit subject: %s
Requirements:
- 2 to 5 short bullet points
- each bullet starts with "- "
- include what changed and why`, strings.TrimSpace(commitMessage))

	resp, err := svc.agent.Run(repo.Path, prompt)
	if err != nil {
		return "", err
	}

	prBody := sanitizeAgentPRBody(resp)
	if prBody == "" {
		return "", errors.New("agent returned an empty pr description")
	}

	return prBody, nil
}

func sanitizeAgentCommitMessage(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	for _, line := range strings.Split(trimmed, "\n") {
		candidate := strings.TrimSpace(line)
		if candidate == "" {
			continue
		}
		if strings.EqualFold(candidate, "new session started.") {
			continue
		}
		lower := strings.ToLower(candidate)
		if strings.HasPrefix(lower, "commit message:") {
			candidate = strings.TrimSpace(candidate[len("commit message:"):])
		}
		candidate = strings.TrimSpace(strings.Trim(candidate, "`\"'"))
		if candidate == "" {
			continue
		}
		return candidate
	}

	return ""
}

func sanitizeAgentPRBody(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	lines := strings.Split(trimmed, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		candidate := strings.TrimSpace(line)
		if candidate == "" {
			continue
		}
		if strings.EqualFold(candidate, "new session started.") {
			continue
		}

		lower := strings.ToLower(candidate)
		if strings.HasPrefix(lower, "pr description:") {
			candidate = strings.TrimSpace(candidate[len("pr description:"):])
		}
		if strings.HasPrefix(candidate, "```") {
			continue
		}

		candidate = strings.TrimSpace(strings.Trim(candidate, "`"))
		if candidate == "" {
			continue
		}

		cleaned = append(cleaned, candidate)
	}

	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

func truncateTelegramText(text string) string {
	const maxLen = 3900

	trimmed := strings.TrimSpace(text)
	if len(trimmed) <= maxLen {
		return trimmed
	}

	return strings.TrimSpace(trimmed[:maxLen]) + "\n\n[output truncated]"
}

func (svc *TelegramService) onRestart(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		log.Warn().Msg("onRestart: nil message")
		return nil
	}
	chat := c.Chat()
	threadID := 0

	opts := &tb.SendOptions{}
	if msg.TopicMessage && msg.ThreadID != 0 {
		threadID = msg.ThreadID
		opts.ThreadID = threadID
	}
	if err := c.Send("Restarting gocode...", opts); err != nil {
		return err
	}

	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := svc.restartProcess(); err != nil {
			log.Error().Err(err).Msg("failed to restart process")
			svc.notifyRestartFailure(chat, threadID, err)
		}
	}()

	return nil
}

type restartFailure struct {
	Command     string
	Err         error
	IsBuildStep bool
}

func (e *restartFailure) Error() string {
	return fmt.Sprintf("restart command failed: %s: %v", e.Command, e.Err)
}

func (e *restartFailure) Unwrap() error {
	return e.Err
}

func (svc *TelegramService) restartProcess() error {
	projectDir, err := svc.resolveProjectDir()
	if err != nil {
		return err
	}

	restartCommands := [][]string{
		{"git", "pull"},
		{"go", "mod", "tidy"},
		{"go", "mod", "vendor"},
		{"go", "build", "./runtime/gocode.go"},
	}
	for _, args := range restartCommands {
		if err := svc.runRestartCommand(projectDir, args...); err != nil {
			command := strings.Join(args, " ")
			return &restartFailure{
				Command:     command,
				Err:         err,
				IsBuildStep: command == "go build ./runtime/gocode.go",
			}
		}
	}

	log.Info().Msg("restart commands completed; terminating process for supervisor restart")
	return syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

func (svc *TelegramService) notifyRestartFailure(chat *tb.Chat, threadID int, err error) {
	if chat == nil {
		log.Warn().Err(err).Msg("restart failed but chat was nil; could not notify user")
		return
	}

	restartErr, ok := err.(*restartFailure)
	if !ok {
		restartErr = &restartFailure{
			Command: "unknown",
			Err:     err,
		}
	}

	opts := &tb.SendOptions{}
	if threadID != 0 {
		opts.ThreadID = threadID
	}

	parts := []string{
		"Restart failed.",
		fmt.Sprintf("Command: %s", restartErr.Command),
		fmt.Sprintf("Error: %s", truncateTelegramText(restartErr.Error())),
	}

	if _, sendErr := svc.sendWithRetry(chat, strings.Join(parts, "\n"), opts); sendErr != nil {
		log.Error().Err(sendErr).Msg("failed to notify restart failure")
	}

	if restartErr.IsBuildStep {
		_, _ = svc.sendWithRetry(chat, "Build failed. Restarting current process to recover.", opts)
		time.Sleep(500 * time.Millisecond)
		if killErr := syscall.Kill(os.Getpid(), syscall.SIGTERM); killErr != nil {
			log.Error().Err(killErr).Msg("failed to terminate process after restart failure")
		}
	}
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
			return err
		}
		return fmt.Errorf("%w: %s", err, out)
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
	_, err = svc.sendWithRetry(c.Chat(), msg, nil)
	return err
}

// escapeMarkdownV2 converts standard Markdown text into Telegram MarkdownV2
// safe text. It walks the input character by character, tracking context
// (code blocks, inline code, link URLs) and applies the correct escaping
// rules for each context per the Telegram Bot API spec.
func escapeMarkdownV2(text string) string {
	// Characters that must be escaped in normal text.
	const specialChars = `_*[]()~` + "`" + `>#+-=|{}.!\`

	isSpecial := func(c byte) bool {
		return strings.IndexByte(specialChars, c) >= 0
	}

	var b strings.Builder
	b.Grow(len(text) + len(text)/4)

	i := 0
	n := len(text)

	for i < n {
		// --- fenced code block: ```...``` ---
		if i+2 < n && text[i] == '`' && text[i+1] == '`' && text[i+2] == '`' {
			// Find the optional language tag (rest of the line after ```)
			j := i + 3
			// Skip the language identifier on the opening fence
			for j < n && text[j] != '\n' && text[j] != '`' {
				j++
			}
			// Find closing ```
			end := strings.Index(text[j:], "```")
			if end == -1 {
				// No closing fence — escape the triple backtick and continue
				b.WriteString("\\`\\`\\`")
				i += 3
				continue
			}
			end += j // absolute index of closing ```

			// Write opening ```
			b.WriteString("```")
			// Write language tag + content, escaping only ` and \ inside
			for k := i + 3; k < end; k++ {
				if text[k] == '`' || text[k] == '\\' {
					b.WriteByte('\\')
				}
				b.WriteByte(text[k])
			}
			// Write closing ```
			b.WriteString("```")
			i = end + 3
			continue
		}

		// --- inline code: `...` ---
		if text[i] == '`' {
			end := strings.IndexByte(text[i+1:], '`')
			if end == -1 {
				// No closing backtick — escape and continue
				b.WriteString("\\`")
				i++
				continue
			}
			end += i + 1 // absolute index of closing `

			b.WriteByte('`')
			// Inside code: escape only ` and \
			for k := i + 1; k < end; k++ {
				if text[k] == '`' || text[k] == '\\' {
					b.WriteByte('\\')
				}
				b.WriteByte(text[k])
			}
			b.WriteByte('`')
			i = end + 1
			continue
		}

		// --- inline link: [text](url) ---
		if text[i] == '[' {
			// Look for ](
			closeBracket := -1
			depth := 1
			for k := i + 1; k < n; k++ {
				if text[k] == '[' {
					depth++
				} else if text[k] == ']' {
					depth--
					if depth == 0 {
						closeBracket = k
						break
					}
				}
			}
			if closeBracket != -1 && closeBracket+1 < n && text[closeBracket+1] == '(' {
				// Find the matching closing )
				parenStart := closeBracket + 2
				parenDepth := 1
				parenEnd := -1
				for k := parenStart; k < n; k++ {
					if text[k] == '(' && text[k-1] != '\\' {
						parenDepth++
					} else if text[k] == ')' {
						parenDepth--
						if parenDepth == 0 {
							parenEnd = k
							break
						}
					}
				}
				if parenEnd != -1 {
					// Write [, escaped link text, ](, escaped URL, )
					b.WriteByte('[')
					for k := i + 1; k < closeBracket; k++ {
						if isSpecial(text[k]) {
							b.WriteByte('\\')
						}
						b.WriteByte(text[k])
					}
					b.WriteString("](")
					// Inside (...): escape only ) and \
					for k := parenStart; k < parenEnd; k++ {
						if text[k] == ')' || text[k] == '\\' {
							b.WriteByte('\\')
						}
						b.WriteByte(text[k])
					}
					b.WriteByte(')')
					i = parenEnd + 1
					continue
				}
			}
			// Not a valid link — escape the [ and continue
			b.WriteString("\\[")
			i++
			continue
		}

		// --- normal text ---
		if isSpecial(text[i]) {
			b.WriteByte('\\')
		}
		b.WriteByte(text[i])
		i++
	}

	return b.String()
}
