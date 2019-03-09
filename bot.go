// Package joe contains a general purpose bot library inspired by Hubot.
package joe

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/pkg/errors"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// A Bot represents an event based chat bot. For the most simple usage you can
// use the Bot.Respond(…) function to make the bot execute a function when it
// receives a message that matches a given pattern.
//
// More advanced usage includes persisting memory or emitting your own events
// using the Brain of the robot.
type Bot struct {
	Name    string
	Adapter Adapter
	Brain   *Brain
	Logger  *zap.Logger

	ctx     context.Context
	initErr error // any error when we created a new bot
}

// A Module is an optional Bot extension that can add new capabilities such as
// a different Brain.Memory implementation or a different Adapter.
type Module func(*Config) error

// New creates a new Bot and initializes it with the given Modules and Options.
// By default the Bot will use an in-memory in Brain and a CLI adapter that
// reads messages from stdin and writes to stdout.
//
// The modules can be used to change the Memory or Adapter or register other new
// functionality. Additionally you can pass Options which allow setting some
// simple configuration such as the event handler timeouts or injecting a
// different context. All Options are available as functions in this package
// that start with "With…".
//
// If there was an error initializing a Module it is stored and returned on the
// next call to Bot.Run(). Before you start the bot however you should register
// your custom event handlers.
//
// Example:
//   b := joe.New("example",
//       redis.Memory("localhost:6379"),
//       slack.Adapter("xoxb-58942365423-…"),
//       joehttp.Server(":8080"),
//       joe.WithHandlerTimeout(time.Second),
//   )
//
//   b.Respond("ping", b.Pong)
//   b.Brain.RegisterHandler(b.Init)
//
//   err := b.Run()
//   …
func New(name string, modules ...Module) *Bot {
	ctx := cliContext() // context can be changed via the WithContext Option/Module.
	logger := newLogger()
	return newBot(ctx, logger, name, modules...)
}

// cliContext creates the default context.Context that is used by the bot.
// This context is canceled if the bot receives a SIGINT, SIGQUIT or SIGTERM.
func cliContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	return ctx
}

func newLogger() *zap.Logger {
	cfg := zap.Config{
		Level:       zap.NewAtomicLevelAt(zap.DebugLevel),
		Development: false,
		Encoding:    "console",
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:        "T",
			LevelKey:       "L",
			NameKey:        "N",
			MessageKey:     "M",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    zapcore.CapitalLevelEncoder,
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeDuration: zapcore.StringDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		},
		OutputPaths:      []string{"stderr"},
		ErrorOutputPaths: []string{"stderr"},
	}

	logger, err := cfg.Build()
	if err != nil {
		panic(err)
	}

	return logger
}

func newBot(ctx context.Context, logger *zap.Logger, name string, modules ...Module) *Bot {
	brain := NewBrain(logger.Named("brain"))

	conf := &Config{
		Context: ctx,
		Name:    name,
		adapter: NewCLIAdapter(name, logger),
		logger:  logger,
		brain:   brain,
	}

	conf.logger.Info("Initializing bot", zap.String("name", name))
	for _, mod := range modules {
		err := mod(conf)
		if err != nil {
			conf.errs = append(conf.errs, err)
		}
	}

	return &Bot{
		Name:    conf.Name,
		ctx:     conf.Context,
		Logger:  conf.logger,
		Adapter: conf.adapter,
		Brain:   brain,
		initErr: multierr.Combine(conf.errs...),
	}
}

// Run starts the bot and runs its event handler loop until the bots context
// is canceled (by default via SIGINT, SIGQUIT or SIGTERM). If there was an
// an error when setting up the Bot via New() or when registering the event
// handlers it will be returned before the bot starts to process any events.
func (b *Bot) Run() error {
	if b.initErr != nil {
		return errors.Wrap(b.initErr, "failed to initialize bot")
	}

	if len(b.Brain.registrationErrs) > 0 {
		errs := multierr.Combine(b.Brain.registrationErrs...)
		return errors.Wrap(errs, "invalid event handlers")
	}

	b.Adapter.RegisterAt(b.Brain)

	go func() {
		// TODO: improve this a bit
		<-b.ctx.Done()
		b.Brain.Shutdown()
	}()

	b.Logger.Info("Bot initialized and ready to operate", zap.String("name", b.Name))
	b.Brain.HandleEvents()

	err := b.Adapter.Close()
	b.Logger.Info("Bot is shutting down", zap.String("name", b.Name))
	if err != nil {
		b.Logger.Info("Error while closing adapter", zap.Error(err))
	}

	return nil
}

// Respond registers an event handler that listens for the ReceiveMessageEvent
// and executes the given function only if the message text matches the given
// message. The message will be matched against the msg string as regular
// expression that must match the entire message in a case insensitive way.
//
// You can use sub matches in the msg which will be passed to the function via
// Message.Matches.
//
// If you need complete control over the regular expression, e.g. because you
// want the patter to match only a substring of the message but not all of it,
// you can use Bot.RespondRegex(…).
func (b *Bot) Respond(msg string, fun func(Message) error) {
	expr := "^" + msg + "$"
	b.RespondRegex(expr, fun)
}

// RespondRegex is like Bot.Respond(…) but gives a little more control over the
// regular expression. However, also with this function messages are matched in
// a case insensitive way.
func (b *Bot) RespondRegex(expr string, fun func(Message) error) {
	if expr == "" {
		return
	}

	if expr[0] == '^' {
		// String starts with the "^" anchor but does it also have the prefix
		// or case insensitive matching?
		if !strings.HasPrefix(expr, "^(?i)") { // TODO: strings.ToLower would be easier?
			expr = "^(?i)" + expr[1:]
		}
	} else {
		// The string is not starting with "^" but maybe it has the prefix for
		// case insensitive matching already?
		if !strings.HasPrefix(expr, "(?i)") {
			expr = "(?i)" + expr
		}
	}

	regex, err := regexp.Compile(expr)
	if err != nil {
		caller := firstExternalCaller()
		err = errors.Wrap(err, caller)
		b.Brain.registrationErrs = append(b.Brain.registrationErrs, err)
		return
	}

	b.Brain.RegisterHandler(func(ctx context.Context, evt ReceiveMessageEvent) error {
		matches := regex.FindStringSubmatch(evt.Text)
		if len(matches) == 0 {
			return nil
		}

		return fun(Message{
			Context: ctx,
			Text:    evt.Text,
			Channel: evt.Channel,
			Matches: matches[1:],
			adapter: b.Adapter,
		})
	})
}

// Say is a helper function to makes the Bot output the message via its Adapter
// (e.g. to the CLI or to Slack). If there is at least one vararg the msg and
// args are formatted using fmt.Sprintf.
func (b *Bot) Say(channel, msg string, args ...interface{}) {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}

	err := b.Adapter.Send(msg, channel)
	if err != nil {
		b.Logger.Error("Failed to send message", zap.Error(err))
	}
}
