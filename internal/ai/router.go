package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/command"
	"github.com/SafwanGanz/Sonicos/internal/speech"
	"github.com/SafwanGanz/Sonicos/internal/transcript"
)

const BotName = "Sonicos"

const (
	historyMaxMessages = 12
	historyTTL         = 30 * time.Minute
)

type historyTurn struct {
	role    string
	content string
}

type historyEntry struct {
	turns    []historyTurn
	loadedAt time.Time
}

type Router struct {
	store    command.AIModeStore
	text     TextProvider
	stt      *transcript.Engine
	tts      speech.Synthesizer
	registry *command.Registry
	logger   *slog.Logger
	timeout  time.Duration

	historyMu sync.Mutex
	history   map[string]*historyEntry
}

func NewRouter(store command.AIModeStore, text TextProvider, stt *transcript.Engine, tts speech.Synthesizer, registry *command.Registry, logger *slog.Logger, timeout time.Duration) *Router {
	if store == nil || registry == nil || logger == nil || text == nil || !text.Available() {
		return nil
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &Router{
		store:    store,
		text:     text,
		stt:      stt,
		tts:      tts,
		registry: registry,
		logger:   logger,
		timeout:  timeout,
		history:  make(map[string]*historyEntry),
	}
}

func Addressed(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return t, false
	}
	fields := strings.Fields(t)
	first := strings.Trim(fields[0], ",.:;!?¡¿\"'()[]")
	if !strings.EqualFold(first, BotName) {
		return t, false
	}
	return strings.TrimSpace(strings.TrimPrefix(t, fields[0])), true
}

func (r *Router) modeEnabled(ctx context.Context, msg command.Message) bool {
	if r == nil || r.store == nil {
		return false
	}
	state, err := r.store.GetState(ctx)
	if err != nil {
		r.logger.Error("ai mode state unreadable; failing safe to disabled", "error", err)
		return false
	}
	if !state.Enabled {
		return false
	}
	if msg.Group {
		groupEnabled, err := r.store.GetGroupEnabled(ctx, msg.Chat)
		if err != nil {
			r.logger.Error("ai group mode unreadable; failing safe to disabled", "group", msg.Chat, "error", err)
			return false
		}
		if !groupEnabled {
			return false
		}
	}
	return true
}

type decision struct {
	Tool  string `json:"tool"`
	Args  string `json:"args"`
	Reply string `json:"reply"`
}

func (r *Router) Handle(ctx context.Context, msg command.Message, reply command.ReplyFunc) (bool, error) {
	if r == nil || r.store == nil {
		return false, nil
	}
	if msg.FromMe {
		return false, nil
	}
	text, addressed := Addressed(msg.Text)
	if !addressed && !msg.QuotedFromMe {
		return false, nil
	}
	if !r.modeEnabled(ctx, msg) {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	userText := text
	wasVoice := false
	if msg.MediaSource != nil {
		attachment, err := msg.MediaSource.Download(ctx)
		if err != nil {
			r.logger.Debug("ai media download failed", "chat", msg.Chat, "error", err)
		} else if attachment.Kind == command.MediaAudio {
			wasVoice = true
			if !r.stt.Available() {
				return true, reply(ctx, command.Reply{Text: "Speech-to-text is not configured, so I can't listen to voice messages yet. Please send text."})
			}
			res, err := r.stt.Transcribe(ctx, attachment.Data, attachment.MIMEType)
			if err != nil {
				r.logger.Error("ai transcription failed", "chat", msg.Chat, "error", err)
				return true, reply(ctx, command.Reply{Text: "I couldn't transcribe that voice message right now. Please try again or send text."})
			}
			userText = res.Text
			if res.Uncertain {
				userText = "[uncertain transcription] " + userText
			}
		}
	}
	if userText == "" {
		return true, reply(ctx, command.Reply{Text: "I'm listening — how can I help?"})
	}

	history := r.takeHistory(msg.Chat)
	messages := make([]Message, 0, len(history)+2)
	for _, turn := range history {
		messages = append(messages, Message{Role: turn.role, Content: turn.content})
	}
	messages = append(messages,
		Message{Role: "user", Content: userText},
		Message{Role: "system", Content: r.systemPrompt()},
	)

	result, err := r.text.Chat(ctx, ChatRequest{Messages: messages, Tools: r.toolSchemas()})
	if err != nil {
		r.logger.Error("ai completion failed", "chat", msg.Chat, "error", err)
		return true, reply(ctx, command.Reply{Text: "The AI backend is unavailable right now. Please try again shortly."})
	}

	r.rememberTurn(msg.Chat, userText, result.Text)

	if len(result.ToolCalls) > 0 {
		call := result.ToolCalls[0]
		return true, r.runTool(ctx, msg, call.Name, call.Arguments, reply)
	}
	d := parseDecision(result.Text)
	if d.Tool != "" {
		return true, r.runTool(ctx, msg, d.Tool, d.Args, reply)
	}
	responseText := d.Reply
	if responseText == "" {
		responseText = result.Text
	}
	return true, r.deliver(ctx, msg, responseText, wasVoice, reply)
}

func (r *Router) runTool(ctx context.Context, msg command.Message, name, args string, reply command.ReplyFunc) error {
	cmd, ok := r.registry.Lookup(name)
	if !ok {
		r.logger.Warn("ai selected unknown tool", "tool", name, "chat", msg.Chat)
		return reply(ctx, command.Reply{Text: "I couldn't find a matching capability for that request."})
	}
	execMsg := msg
	execMsg.Text = args
	var captured command.Reply
	capture := func(_ context.Context, rep command.Reply) error {
		captured = rep
		return nil
	}
	if err := cmd.Handler(ctx, execMsg, strings.Fields(args), capture); err != nil {
		r.logger.Error("ai tool execution failed", "tool", name, "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "That action failed. Please try again."})
	}
	if isEmptyReply(captured) {
		captured = command.Reply{Text: "Done."}
	}
	return reply(ctx, captured)
}

func (r *Router) deliver(ctx context.Context, msg command.Message, text string, wasVoice bool, reply command.ReplyFunc) error {
	if wasVoice && r.tts != nil && r.tts.Available() {
		if ptt, ok := r.tts.(speech.PTTSynthesizer); ok {
			ogg, seconds, waveform, err := ptt.SynthesizePTT(ctx, text)
			if err == nil && len(ogg) > 0 {
				return reply(ctx, command.Reply{Audio: &command.AudioReply{
					Data: ogg, MIMEType: "audio/ogg; codecs=opus", Seconds: seconds, Waveform: waveform, PTT: true,
				}})
			}
			r.logger.Warn("voice reply synthesis failed; falling back to text", "chat", msg.Chat, "error", err)
			return reply(ctx, command.Reply{Text: text})
		}
		audio, err := r.tts.Synthesize(ctx, text)
		if err == nil && len(audio) > 0 {
			return reply(ctx, command.Reply{Audio: &command.AudioReply{Data: audio, MIMEType: "audio/mpeg"}})
		}
		r.logger.Warn("voice reply synthesis failed; falling back to text", "chat", msg.Chat, "error", err)
	}
	return reply(ctx, command.Reply{Text: text})
}

func (r *Router) toolSchemas() []ToolSchema {
	commands := r.registry.Commands()
	schemas := make([]ToolSchema, 0, len(commands))
	for _, c := range commands {
		schemas = append(schemas, ToolSchema{Name: c.Name, Description: c.Description})
	}
	return schemas
}

func (r *Router) systemPrompt() string {
	return fmt.Sprintf(`You are %s, a helpful WhatsApp assistant. The user is talking to you in natural language without any prefix.

Decide the single best action for the user's request. Use a provided tool when the request clearly maps to one of the available capabilities; otherwise answer directly in natural language.

Rules:
- Keep tool arguments exactly as the user intended (names, numbers, quoted text).
- Reply in the same language the user used.
- Never invent capabilities that are not provided as tools.
- Conversation history is provided above; use it to resolve context.`, BotName)
}

func (r *Router) takeHistory(chat string) []historyTurn {
	r.historyMu.Lock()
	defer r.historyMu.Unlock()
	entry, ok := r.history[chat]
	if !ok || time.Since(entry.loadedAt) > historyTTL {
		return nil
	}
	out := append([]historyTurn(nil), entry.turns...)
	return out
}

func (r *Router) rememberTurn(chat, user, assistant string) {
	r.historyMu.Lock()
	defer r.historyMu.Unlock()
	entry, ok := r.history[chat]
	if !ok || time.Since(entry.loadedAt) > historyTTL {
		entry = &historyEntry{}
		r.history[chat] = entry
	}
	entry.loadedAt = time.Now()
	entry.turns = append(entry.turns, historyTurn{role: "user", content: user}, historyTurn{role: "assistant", content: assistant})
	if len(entry.turns) > historyMaxMessages {
		entry.turns = entry.turns[len(entry.turns)-historyMaxMessages:]
	}
}

func parseDecision(raw string) decision {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	var d decision
	if err := json.Unmarshal([]byte(s), &d); err == nil {
		d.Tool = strings.TrimSpace(d.Tool)
		d.Args = strings.TrimSpace(d.Args)
		d.Reply = strings.TrimSpace(d.Reply)
		if d.Tool != "" || d.Reply != "" {
			return d
		}
	}
	return decision{Reply: strings.TrimSpace(raw)}
}

func isEmptyReply(r command.Reply) bool {
	return r.Text == "" && r.Sticker == nil && r.Image == nil && r.Audio == nil &&
		r.Video == nil && r.VideoNote == nil && r.StickerReaction == nil
}
