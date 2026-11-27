package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/command"
)

type Idempotency interface {
	Begin(context.Context, string) (string, bool, error)
	Complete(context.Context, string, string) error
	Release(context.Context, string, string) error
	Close() error
}

type Reply = command.ReplyFunc

type AIRouter interface {
	Handle(context.Context, command.Message, Reply) (bool, error)
}

type Processor struct {
	registry    *command.Registry
	idempotency Idempotency
	prefix      string
	timeout     time.Duration
	aiTimeout   time.Duration
	ai          AIRouter
	logger      *slog.Logger
}

func NewProcessor(registry *command.Registry, idempotency Idempotency, prefix string, timeout time.Duration, logger *slog.Logger) (*Processor, error) {
	if registry == nil || idempotency == nil || logger == nil {
		return nil, fmt.Errorf("command registry, idempotency store, and logger are required")
	}
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("command prefix cannot be empty")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("command timeout must be positive")
	}
	return &Processor{
		registry:    registry,
		idempotency: idempotency,
		prefix:      prefix,
		timeout:     timeout,
		aiTimeout:   2 * time.Minute,
		logger:      logger,
	}, nil
}

func (p *Processor) SetAIRouter(router AIRouter) {
	p.ai = router
}

func (p *Processor) Handle(parent context.Context, message command.Message, reply Reply) error {
	text := strings.TrimSpace(message.Text)
	isCommand := text != "" && strings.HasPrefix(text, p.prefix)
	watchers := p.registry.Watchers()
	aiCandidate := p.ai != nil && !isCommand && !message.FromMe
	shouldWatch := !isCommand && message.Group && !message.FromMe && len(watchers) > 0
	if !isCommand && !shouldWatch && !aiCandidate {
		return nil
	}

	timeout := p.timeout
	if aiCandidate && !isCommand && !shouldWatch {
		timeout = p.aiTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	key := message.Chat + ":" + message.ID
	token, claimed, err := p.idempotency.Begin(ctx, key)
	if err != nil {
		return fmt.Errorf("claim message processing: %w", err)
	}
	if !claimed {
		return nil
	}

	completed := false
	defer func() {
		if completed {
			return
		}
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer releaseCancel()
		if err := p.idempotency.Release(releaseCtx, key, token); err != nil {
			p.logger.Error("release message idempotency key failed", "message_id", message.ID, "error", err)
		}
	}()

	var dispatched bool
	if isCommand {
		dispatched, err = p.registry.Dispatch(ctx, p.prefix, message, reply)
		if err != nil {
			return fmt.Errorf("dispatch command: %w", err)
		}
		if !dispatched {
			p.logger.Warn("unknown bot command", "chat", message.Chat, "message_id", message.ID)
			if err := reply(ctx, command.Reply{Text: fmt.Sprintf("Unknown command. Send %shelp to see available commands.", p.prefix)}); err != nil {
				return fmt.Errorf("reply to unknown command: %w", err)
			}
		}
	} else {
		for _, w := range watchers {
			if err := w(ctx, message, reply); err != nil {
				return fmt.Errorf("run message watcher: %w", err)
			}
		}
		if p.ai != nil {
			handled, err := p.ai.Handle(ctx, message, reply)
			if err != nil {
				return fmt.Errorf("run ai router: %w", err)
			}
			if handled {
				dispatched = true
			}
		}
	}

	if err := p.idempotency.Complete(ctx, key, token); err != nil {
		return fmt.Errorf("complete message processing: %w", err)
	}
	completed = true
	if isCommand && dispatched {
		p.logger.Info("command handled", "command", strings.Fields(strings.TrimPrefix(text, p.prefix))[0], "chat", message.Chat, "message_id", message.ID)
	}
	return nil
}
