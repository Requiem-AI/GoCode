package services

import (
	"errors"
	"fmt"
	"os"
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

	svc.Bot.Handle(tb.OnText, svc.onText)
	//svc.Bot.Handle(tb.OnCallback, svc.onCallback)
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
		return c.Send("Use /new <name> to create a topic, then chat inside that topic.")
	}

	log.Info().Str("text", msg.Text).Int("topic", msg.ThreadID).Msg("onText")

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

	_ = svc.Bot.Notify(c.Chat(), tb.Typing, c.Topic().ThreadID)

	resp, err := svc.agent.Run(repo.Path, c.Text())

	if err != nil {
		log.Error().Err(err).Msg("failed to run agent request")
		return c.Send("Agent failed to run.")
	}

	_, err = svc.Bot.Send(c.Chat(),
		resp,
		&tb.SendOptions{ThreadID: msg.ThreadID, ParseMode: tb.ModeMarkdown})
	return err
}

func (svc *TelegramService) onStart(c tb.Context) error {
	log.Info().Str("text", c.Text()).Msg("onStart")
	return c.Send("Create a topic with /topic <name>, then chat inside that topic.")
}

func (svc *TelegramService) onClear(c tb.Context) error {
	msg := c.Message()
	if msg == nil || !msg.TopicMessage || msg.ThreadID == 0 {
		return c.Send("Use /clear inside a topic to reset the context.")
	}

	log.Info().Int("topic", msg.ThreadID).Msg("onClear")

	//svc.clearContext(msg.Chat.ID, msg.ThreadID)
	_, err := svc.Bot.Send(c.Chat(), "Context cleared.", &tb.SendOptions{ThreadID: msg.ThreadID})
	return err
}

func (svc *TelegramService) onTopic(c tb.Context) error {
	msg := c.Message()
	if msg == nil {
		return nil
	}

	name := strings.TrimSpace(msg.Payload)
	if name == "" {
		return c.Send("Usage: /topic <name>")
	}

	topic, err := svc.Bot.CreateTopic(c.Chat(), &tb.Topic{
		Name: name,
	})
	if err != nil {
		log.Error().Err(err).Msg("failed to create topic")
		return c.Send("Couldn't create the topic.")
	}

	repo, err := svc.ensureRepo(c.Chat(), topic.ThreadID)
	if err != nil {
		log.Error().Err(err).Msg("failed to create repo")
		return c.Send("Topic created, but repo creation failed.")
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

func (svc *TelegramService) createFeatureBranch(repo *GitRepo, feature string) (string, error) {
	if svc.git == nil {
		return "", errors.New("git service not available")
	}

	return svc.git.CreateFeatureBranch(repo, feature)
}
