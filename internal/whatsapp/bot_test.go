package whatsapp

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/command"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t.UTC()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC()
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestBot(clock *fakeClock) *Bot {
	now := clock.Now()
	return &Bot{
		queue:           make(chan *events.Message, 64),
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		qrSnap:          QRSnapshot{State: "disconnected"},
		acceptAfter:     now,
		awaitingConnect: true,
		now:             clock.Now,
	}
}

func makeGroupMessage(id string, ts time.Time) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:    types.NewJID("120363000000000000", types.GroupServer),
				Sender:  types.NewJID("15551234567", types.DefaultUserServer),
				IsGroup: true,
			},
			ID:        types.MessageID(id),
			Timestamp: ts,
		},
		Message: &waE2E.Message{},
	}
}

func drainQueueIDs(q chan *events.Message) []string {
	var ids []string
	for {
		select {
		case evt := <-q:
			ids = append(ids, string(evt.Info.ID))
		default:
			return ids
		}
	}
}

func TestHandleEvent_IgnoresMessagesBeforeConnectAndAcceptsAfterConnect(t *testing.T) {
	bootTime := time.Date(2026, 11, 26, 12, 0, 0, 500_000_000, time.UTC)
	clock := newFakeClock(bootTime)
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())

	clock.Advance(5 * time.Second)
	connectTime := clock.Now()
	handler(&events.Connected{})

	handler(makeGroupMessage("old-1", bootTime.Add(-10*time.Minute)))
	handler(makeGroupMessage("old-2", connectTime.Add(-10*time.Second)))

	clock.Advance(1 * time.Second)
	handler(makeGroupMessage("live-1", clock.Now()))

	got := drainQueueIDs(bot.queue)
	if len(got) != 1 || got[0] != "live-1" {
		t.Fatalf("expected [live-1], got %v", got)
	}
}

func TestHandleEvent_IgnoresOfflineSyncWindowAndAcceptsAfterCompleted(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())

	handler(&events.OfflineSyncPreview{Total: 3, Messages: 3})

	handler(makeGroupMessage("sync-old", clock.Now().Add(-5*time.Minute)))
	handler(makeGroupMessage("sync-boundary", clock.Now()))

	handler(&events.Connected{})

	handler(makeGroupMessage("sync-still-active", clock.Now()))

	handler(&events.OfflineSyncCompleted{Count: 3})
	clock.Advance(1 * time.Second)
	handler(makeGroupMessage("live-after-sync", clock.Now()))

	got := drainQueueIDs(bot.queue)
	if len(got) != 1 || got[0] != "live-after-sync" {
		t.Fatalf("expected [live-after-sync], got %v", got)
	}
}

func TestHandleEvent_ReceiptsOnlyOfflineSyncDoesNotBlockLiveMessages(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())

	handler(&events.OfflineSyncPreview{Total: 200, Messages: 0})
	handler(&events.Connected{})
	handler(makeGroupMessage("live-with-receipts-only", clock.Now()))

	got := drainQueueIDs(bot.queue)
	if len(got) != 1 || got[0] != "live-with-receipts-only" {
		t.Fatalf("expected [live-with-receipts-only], got %v", got)
	}
}

func TestHandleEvent_ClearsOfflineSyncAfterExpectedMessageCountBeforeCompletedEvent(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())

	handler(&events.Connected{})
	handler(&events.OfflineSyncPreview{Total: 200, Messages: 2})

	handler(makeGroupMessage("offline-1", clock.Now().Add(-5*time.Minute)))
	handler(makeGroupMessage("offline-2", clock.Now().Add(-1*time.Minute)))

	handler(makeGroupMessage("live-right-after-2", clock.Now()))

	got := drainQueueIDs(bot.queue)
	if len(got) != 1 || got[0] != "live-right-after-2" {
		t.Fatalf("expected [live-right-after-2], got %v", got)
	}
}

func TestHandleEvent_ClearlyLiveMessageBypassesOfflineSyncWithoutConsumingQuota(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())

	handler(&events.Connected{})
	handler(&events.OfflineSyncPreview{Total: 200, Messages: 2})

	handler(makeGroupMessage("offline-1", clock.Now().Add(-10*time.Minute)))

	liveTS := clock.Now().Add(incomingSkewTolerance + time.Second)
	handler(makeGroupMessage("live-during-sync", liveTS))

	handler(makeGroupMessage("offline-2", clock.Now().Add(-2*time.Minute)))

	handler(makeGroupMessage("live-after-quota", clock.Now()))

	got := drainQueueIDs(bot.queue)
	if len(got) != 2 || got[0] != "live-during-sync" || got[1] != "live-after-quota" {
		t.Fatalf("expected [live-during-sync live-after-quota], got %v", got)
	}
}

func TestHandleEvent_ConnectedBeforeOfflineSyncPreview(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())

	handler(&events.Connected{})
	handler(&events.OfflineSyncPreview{Total: 2, Messages: 2})
	handler(makeGroupMessage("sync-msg", clock.Now()))
	handler(&events.OfflineSyncCompleted{Count: 2})

	clock.Advance(2 * time.Second)
	handler(makeGroupMessage("live-msg", clock.Now()))

	got := drainQueueIDs(bot.queue)
	if len(got) != 1 || got[0] != "live-msg" {
		t.Fatalf("expected [live-msg], got %v", got)
	}
}

func TestHandleEvent_SubSecondAndSmallSkewTolerance(t *testing.T) {

	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 5, 900_000_000, time.UTC))
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())
	handler(&events.Connected{})

	handler(makeGroupMessage("skew-ok", time.Date(2026, 11, 26, 12, 0, 4, 0, time.UTC)))

	handler(makeGroupMessage("skew-too-old", time.Date(2026, 11, 26, 12, 0, 2, 0, time.UTC)))

	got := drainQueueIDs(bot.queue)
	if len(got) != 1 || got[0] != "skew-ok" {
		t.Fatalf("expected [skew-ok], got %v", got)
	}
}

func TestHandleEvent_RetriedOldOfflineMessageAfterSyncCompletedIsIgnored(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())

	handler(&events.Connected{})
	handler(&events.OfflineSyncPreview{Total: 1, Messages: 1})
	handler(&events.OfflineSyncCompleted{Count: 1})

	retried := makeGroupMessage("retried-old", clock.Now().Add(-15*time.Minute))
	retried.RetryCount = 1
	handler(retried)

	got := drainQueueIDs(bot.queue)
	if len(got) != 0 {
		t.Fatalf("expected retried old message to be ignored, got %v", got)
	}
}

func TestShouldIgnoreMessage_StatelessFilters(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())
	handler(&events.Connected{})

	handler((*events.Message)(nil))

	broadcast := makeGroupMessage("broadcast-1", now)
	broadcast.Info.Chat = types.NewJID("123456", types.BroadcastServer)
	handler(broadcast)

	historyMsg := makeGroupMessage("history-1", now)
	historyMsg.SourceWebMsg = &waWeb.WebMessageInfo{}
	handler(historyMsg)

	placeholderMsg := makeGroupMessage("placeholder-1", now)
	placeholderMsg.UnavailableRequestID = "req-123"
	handler(placeholderMsg)

	got := drainQueueIDs(bot.queue)
	if len(got) != 0 {
		t.Fatalf("expected all filtered events to be ignored, got %v", got)
	}
}

func TestHandleEvent_ReconnectAfterDowntimeDropsOfflineMessagesBeforeConnected(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())

	handler(&events.Connected{})
	handler(makeGroupMessage("before-disconnect", clock.Now()))

	handler(&events.Disconnected{})
	sentWhileOffline := clock.Now().Add(10 * time.Minute)

	clock.Advance(30 * time.Minute)
	bot.markConnecting()

	handler(makeGroupMessage("offline-while-down", sentWhileOffline))

	handler(&events.Connected{})
	handler(makeGroupMessage("fresh-after-reconnect", clock.Now()))

	got := drainQueueIDs(bot.queue)
	if len(got) != 2 || got[0] != "before-disconnect" || got[1] != "fresh-after-reconnect" {
		t.Fatalf("expected [before-disconnect fresh-after-reconnect], got %v", got)
	}
}

func TestHandleEvent_DisconnectMidOfflineSyncResetsState(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())

	handler(&events.OfflineSyncPreview{Total: 5, Messages: 5})
	handler(&events.Disconnected{})

	clock.Advance(5 * time.Second)
	handler(&events.Connected{})
	handler(makeGroupMessage("after-reset", clock.Now()))

	got := drainQueueIDs(bot.queue)
	if len(got) != 1 || got[0] != "after-reset" {
		t.Fatalf("expected [after-reset], got %v", got)
	}
}

func TestHandleEvent_ZeroOfflineSyncPreviewDoesNotBlock(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	bot := newTestBot(clock)
	handler := bot.handleEvent(context.Background())

	handler(&events.OfflineSyncPreview{Total: 0, Messages: 0})
	handler(&events.Connected{})
	handler(makeGroupMessage("live-zero-preview", clock.Now()))

	got := drainQueueIDs(bot.queue)
	if len(got) != 1 || got[0] != "live-zero-preview" {
		t.Fatalf("expected [live-zero-preview], got %v", got)
	}
}

func TestHandleEvent_ConcurrentRaceFree(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	bot := newTestBot(clock)
	bot.queue = make(chan *events.Message, 256)
	handler := bot.handleEvent(context.Background())

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := range 25 {
				switch j % 5 {
				case 0:
					handler(&events.Connected{})
				case 1:
					handler(&events.OfflineSyncPreview{Total: 1, Messages: 1})
				case 2:
					handler(&events.OfflineSyncCompleted{Count: 1})
				case 3:
					handler(&events.Disconnected{})
				default:
					handler(makeGroupMessage("msg", clock.Now()))
				}
				if worker%2 == 0 {
					clock.Advance(10 * time.Millisecond)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestGroupServiceCachePrimeGetInfoAndEventInvalidate(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	bot := newTestBot(clock)
	gs := &groupService{cache: make(map[string]cachedGroupInfo)}
	bot.groups = gs

	groupJID := types.NewJID("120363000123456789", types.GroupServer)
	gs.Prime(&types.GroupInfo{
		JID:       groupJID,
		GroupName: types.GroupName{Name: "Test Group"},
		Participants: []types.GroupParticipant{
			{JID: types.NewJID("15550001111", types.DefaultUserServer), IsAdmin: true},
		},
	})

	info, err := gs.GetInfo(context.Background(), groupJID.String())
	if err != nil {
		t.Fatalf("GetInfo cached hit failed: %v", err)
	}
	if info.Name != "Test Group" || len(info.Participants) != 1 || !info.Participants[0].IsAdmin {
		t.Fatalf("unexpected cached group info: %+v", info)
	}

	handler := bot.handleEvent(context.Background())
	handler(&events.GroupInfo{JID: groupJID})

	if _, err := gs.GetInfo(context.Background(), groupJID.String()); err == nil {
		t.Fatal("expected cache miss and nil-client error after GroupInfo invalidation")
	}
}

type trackingModStore struct {
	command.ModerationStore
	loadStageCalls atomic.Int32
}

func (s *trackingModStore) LoadStage(context.Context, string) (command.StagedMedia, bool, error) {
	s.loadStageCalls.Add(1)
	return command.StagedMedia{}, false, nil
}

func (s *trackingModStore) SaveStage(context.Context, string, command.StagedMedia, int) error {
	return nil
}

func TestHydrateQuotedSoundStickerSkipsSQLForPlainRepliesAndCommands(t *testing.T) {
	store := &trackingModStore{}
	bot := &Bot{
		moderation: store,
		audioCache: newStickerAudioCache(),
	}

	for _, txt := range []string{"just replying to something", "!ping", "!help", "!kick @user"} {
		msg := command.Message{
			Chat:            "1203630001@g.us",
			Text:            txt,
			QuotedMessageID: "quoted-1",
			ModerationStore: store,
		}
		bot.hydrateQuotedSoundSticker(context.Background(), &msg)
	}
	if got := store.loadStageCalls.Load(); got != 0 {
		t.Fatalf("expected 0 SQL LoadStage calls for plain quoted replies/commands, got %d", got)
	}

	stealMsg := command.Message{
		Chat:            "1203630001@g.us",
		Text:            "!steal MyPack",
		QuotedMessageID: "quoted-2",
		ModerationStore: store,
	}
	bot.hydrateQuotedSoundSticker(context.Background(), &stealMsg)
	if got := store.loadStageCalls.Load(); got == 0 {
		t.Fatal("expected SQL LoadStage fallback for !steal")
	}

	before := store.loadStageCalls.Load()
	quotedStickerMsg := command.Message{
		Chat:            "1203630001@g.us",
		Text:            "nice sticker",
		QuotedMessageID: "quoted-3",
		QuotedSticker:   &command.StickerInfo{FileSHA256: "abc123"},
		ModerationStore: store,
	}
	bot.hydrateQuotedSoundSticker(context.Background(), &quotedStickerMsg)
	if got := store.loadStageCalls.Load(); got <= before {
		t.Fatal("expected SQL LoadStage fallback when QuotedSticker is present")
	}
}
