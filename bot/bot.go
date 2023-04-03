package bot

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/slack-go/slack"
)

// Config is the configuration for the Slack bot.
type Config struct {
	Debug bool
	Logf  func(string, ...any)

	BotToken string // xoxb-...
	AppToken string // xapp-...
}

// SlackBot is a Slack bot.
type SlackBot struct {
	logf func(string, ...any)

	config *Config
	client *socketmode.Client
	api    *slack.Client
}

func NewSlackBot(config *Config) (*SlackBot, error) {
	if config.AppToken == "" {
		config.AppToken = os.Getenv("SLACK_APP_TOKEN")
	}

	if config.AppToken == "" {
		return nil, fmt.Errorf("SLACK_APP_TOKEN must be set")
	}

	if !strings.HasPrefix(config.AppToken, "xapp-") {
		return nil, fmt.Errorf("SLACK_APP_TOKEN must have the prefix \"xapp-\".")
	}

	if config.BotToken == "" {
		config.BotToken = os.Getenv("SLACK_BOT_TOKEN")
	}

	if config.BotToken == "" {
		return nil, fmt.Errorf("SLACK_BOT_TOKEN must be set")
	}

	if !strings.HasPrefix(config.BotToken, "xoxb-") {
		return nil, fmt.Errorf("SLACK_BOT_TOKEN must have the prefix \"xoxb-\".")
	}

	api := slack.New(
		config.BotToken,
		slack.OptionDebug(config.Debug),
		slack.OptionLog(log.New(os.Stdout, "api: ", log.Lshortfile|log.LstdFlags)),
		slack.OptionAppLevelToken(config.AppToken),
	)

	client := socketmode.New(
		api,
		socketmode.OptionDebug(config.Debug),
		socketmode.OptionLog(log.New(os.Stdout, "socketmode: ", log.Lshortfile|log.LstdFlags)),
	)

	logf := config.Logf
	if logf == nil {
		logf = log.Printf
	}

	return &SlackBot{
		logf:   logf,
		config: config,
		api:    api,
		client: client,
	}, nil
}

func (b *SlackBot) handleEvents() {
	for evt := range b.client.Events {
		switch evt.Type {
		case socketmode.EventTypeConnecting:
			b.logf("Connecting to Slack with Socket Mode...")
		case socketmode.EventTypeConnectionError:
			b.logf("Connection failed. Retrying later...")
		case socketmode.EventTypeConnected:
			b.logf("Connected to Slack with Socket Mode.")
		case socketmode.EventTypeEventsAPI:
			eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
			if !ok {
				b.logf("Ignored %+v", evt)

				continue
			}

			b.logf("Event received: %+v", eventsAPIEvent)

			b.client.Ack(*evt.Request)

			switch eventsAPIEvent.Type {
			case slackevents.CallbackEvent:
				innerEvent := eventsAPIEvent.InnerEvent
				switch ev := innerEvent.Data.(type) {
				case *slackevents.AppMentionEvent:
					_, _, err := b.api.PostMessage(ev.Channel, slack.MsgOptionText("Yes, hello.", false))
					if err != nil {
						b.logf("failed posting message: %v", err)
					}
				case *slackevents.MemberJoinedChannelEvent:
					b.logf("user %q joined to channel %q", ev.User, ev.Channel)
				}
			default:
				b.client.Debugf("unsupported Events API event received")
			}
		case socketmode.EventTypeInteractive:
			callback, ok := evt.Data.(slack.InteractionCallback)
			if !ok {
				b.logf("Ignored %+v", evt)

				continue
			}

			b.logf("Interaction received: %+v", callback)

			var payload interface{}

			switch callback.Type {
			case slack.InteractionTypeBlockActions:
				// See https://api.slack.com/apis/connections/socket-implement#button

				b.client.Debugf("button clicked!")
			case slack.InteractionTypeShortcut:
			case slack.InteractionTypeViewSubmission:
				// See https://api.slack.com/apis/connections/socket-implement#modal
			case slack.InteractionTypeDialogSubmission:
			default:

			}

			b.client.Ack(*evt.Request, payload)
		case socketmode.EventTypeSlashCommand:
			cmd, ok := evt.Data.(slack.SlashCommand)
			if !ok {
				b.logf("Ignored %+v", evt)

				continue
			}

			b.client.Debugf("Slash command received: %+v", cmd)

			payload := map[string]interface{}{
				"blocks": []slack.Block{
					slack.NewSectionBlock(
						&slack.TextBlockObject{
							Type: slack.MarkdownType,
							Text: "foo",
						},
						nil,
						slack.NewAccessory(
							slack.NewButtonBlockElement(
								"",
								"somevalue",
								&slack.TextBlockObject{
									Type: slack.PlainTextType,
									Text: "bar",
								},
							),
						),
					),
				}}

			b.client.Ack(*evt.Request, payload)
		default:
			b.logf("Unexpected event type received: %s", evt.Type)
		}
	}
}

func (b *SlackBot) Run() error {
	go b.handleEvents()
	return b.client.Run()
}
