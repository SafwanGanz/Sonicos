package plugins

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/SafwanGanz/Sonicos/internal/command"
	"github.com/SafwanGanz/Sonicos/internal/owner"
)

func init() {
	command.Register(command.Command{
		Name:        "ai",
		Aliases:     []string{"aimode", "aidriven"},
		Category:    "General",
		Description: "Owner-only AI Driven mode control: on, off, status, group on/off",
		Handler:     aiModeCommand,
	})
}

func aiModeCommand(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	store := msg.AIModeStore
	if store == nil {
		return reply(ctx, command.Reply{Text: "AI mode storage is unavailable right now."})
	}
	action := strings.ToLower(strings.TrimSpace(strings.Join(args, " ")))
	switch action {
	case "", "status":
		return aiModeStatus(ctx, msg, reply)
	case "driven on", "on":
		return aiModeSetGlobal(ctx, msg, reply, true)
	case "driven off", "off":
		return aiModeSetGlobal(ctx, msg, reply, false)
	case "group on":
		return aiModeSetGroup(ctx, msg, reply, true)
	case "group off":
		return aiModeSetGroup(ctx, msg, reply, false)
	case "group", "group status":
		return aiModeGroupStatus(ctx, msg, reply)
	default:
		return reply(ctx, command.Reply{Text: "Usage: AI Driven ON | AI Driven OFF | AI status | AI group ON | AI group OFF"})
	}
}

func aiModeSetGlobal(ctx context.Context, msg command.Message, reply command.ReplyFunc, enabled bool) error {
	store := msg.AIModeStore
	if !owner.IsOwner(msg.Sender) {
		return reply(ctx, command.Reply{Text: "Only the bot owner can change the global AI Driven mode."})
	}
	if enabled && strings.TrimSpace(os.Getenv("AI_API_KEY")) == "" {
		return reply(ctx, command.Reply{Text: "The AI backend is not configured. Set AI_API_KEY (and optionally AI_MODEL) before enabling AI Driven mode."})
	}
	ownerID := owner.Normalize(msg.Sender)
	if err := store.SetState(ctx, enabled, ownerID); err != nil {
		return reply(ctx, command.Reply{Text: "Couldn't persist the AI mode change. Please try again."})
	}
	state, err := store.GetState(ctx)
	if err != nil || state.Enabled != enabled {
		return reply(ctx, command.Reply{Text: "The AI mode change could not be verified as persisted; the safe default remains OFF."})
	}
	if enabled {
		return reply(ctx, command.Reply{Text: "AI Driven mode is ON and persisted. Talk to me naturally — for example: \"Sonicos, help me with this.\" Prefix commands still work as before."})
	}
	return reply(ctx, command.Reply{Text: "AI Driven mode is OFF and persisted. Natural-language processing is disabled; prefix commands (e.g. !help) remain available."})
}

func aiModeSetGroup(ctx context.Context, msg command.Message, reply command.ReplyFunc, enabled bool) error {
	store := msg.AIModeStore
	if !msg.Group {
		return reply(ctx, command.Reply{Text: "Group-level AI controls can only be used in a group."})
	}
	info, ok, err := requireGroupAdmin(ctx, msg, reply, false)
	if !ok || err != nil {
		return err
	}
	_ = info
	state, err := store.GetState(ctx)
	if err != nil {
		return reply(ctx, command.Reply{Text: "Couldn't read the global AI mode right now."})
	}
	if !state.Enabled {
		return reply(ctx, command.Reply{Text: "Global AI Driven mode is OFF. The bot owner must run AI Driven ON first; a group setting can never override global OFF."})
	}
	if err := store.SetGroupEnabled(ctx, msg.Chat, enabled); err != nil {
		return reply(ctx, command.Reply{Text: "Couldn't persist the group AI setting. Please try again."})
	}
	groupEnabled, err := store.GetGroupEnabled(ctx, msg.Chat)
	if err != nil || groupEnabled != enabled {
		return reply(ctx, command.Reply{Text: "The group AI setting could not be verified as persisted."})
	}
	if enabled {
		return reply(ctx, command.Reply{Text: "AI Driven mode is enabled for this group and persisted."})
	}
	return reply(ctx, command.Reply{Text: "AI Driven mode is disabled for this group and persisted."})
}

func aiModeStatus(ctx context.Context, msg command.Message, reply command.ReplyFunc) error {
	store := msg.AIModeStore
	state, err := store.GetState(ctx)
	if err != nil {
		return reply(ctx, command.Reply{Text: "AI mode state could not be read; the safe default is OFF."})
	}
	backend := "not configured (set AI_API_KEY)"
	if strings.TrimSpace(os.Getenv("AI_API_KEY")) != "" {
		backend = "configured"
	}
	stt := "not configured (set GEMINI_API_KEY for free voice transcription)"
	if strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != "" {
		stt = "configured (gemini-native)"
	}
	owners := len(owner.List())
	var b strings.Builder
	if state.Enabled {
		b.WriteString("AI Driven mode: *ON* (persisted)\n")
		b.WriteString("Natural-language processing: *active* when addressed to Sonicos\n")
	} else {
		b.WriteString("AI Driven mode: *OFF* (persisted)\n")
		b.WriteString("Natural-language processing: *disabled*\n")
	}
	fmt.Fprintf(&b, "AI backend: %s\n", backend)
	fmt.Fprintf(&b, "Speech-to-text: %s\n", stt)
	fmt.Fprintf(&b, "Authorized owners configured: %d\n", owners)
	if msg.Group {
		groupEnabled, gerr := store.GetGroupEnabled(ctx, msg.Chat)
		switch {
		case gerr != nil:
			b.WriteString("This group: state unreadable (treated as disabled)\n")
		case !state.Enabled:
			b.WriteString("This group: AI *disabled* (global OFF)\n")
		case groupEnabled:
			b.WriteString("This group: AI *enabled*\n")
		default:
			b.WriteString("This group: AI *disabled*\n")
		}
	}
	return reply(ctx, command.Reply{Text: b.String()})
}

func aiModeGroupStatus(ctx context.Context, msg command.Message, reply command.ReplyFunc) error {
	if !msg.Group {
		return reply(ctx, command.Reply{Text: "This command can only be used in a group."})
	}
	return aiModeStatus(ctx, msg, reply)
}
