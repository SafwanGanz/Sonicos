package whatsapp

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/application"
	"github.com/SafwanGanz/Sonicos/internal/command"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const incomingSkewTolerance = 2 * time.Second

type Bot struct {
	client     *whatsmeow.Client
	processor  *application.Processor
	moderation command.ModerationStore
	groups     *groupService
	audioCache *stickerAudioCache
	fastStore  *fastDeviceContainer
	workers    int
	queue      chan *events.Message
	logger     *slog.Logger

	autoApproveRequests bool
	blockedNumbers      []string

	sessionOpMu sync.Mutex
	pairMu      sync.RWMutex
	runCtx      context.Context
	qrCancel    context.CancelFunc
	qrSnap      QRSnapshot
	qrReady     chan struct{}
	connectedAt time.Time

	syncMu                   sync.RWMutex
	acceptAfter              time.Time
	offlineSyncing           bool
	offlineMessagesRemaining int
	awaitingConnect          bool
	now                      func() time.Time
	warmingUp                atomic.Bool

	listenerMu sync.RWMutex
	listener   EventListener
}

func (b *Bot) SetModerationStore(store command.ModerationStore) {
	b.moderation = store
}

// EnableFastStore wraps the underlying whatsmeow device stores with in-memory
// read-through caches and batched Postgres writes so cross-region database RTT
// does not stall message decryption, sending, or prekey uploads.
func (b *Bot) EnableFastStore(ctx context.Context, db *sql.DB) {
	if b == nil || b.client == nil || b.client.Store == nil || db == nil {
		return
	}
	b.fastStore = attachFastStore(ctx, b.client.Store, db, b.logger)
}

// FlushStore synchronously flushes any pending cached whatsmeow store writes.
func (b *Bot) FlushStore(ctx context.Context) error {
	if b == nil || b.fastStore == nil {
		return nil
	}
	return b.fastStore.Flush(ctx)
}

// SetEventListener registers a callback invoked on WhatsApp connection, QR, and pairing state changes.
func (b *Bot) SetEventListener(fn EventListener) {
	b.listenerMu.Lock()
	b.listener = fn
	b.listenerMu.Unlock()
}

func (b *Bot) emitEvent(event string, data any) {
	b.listenerMu.RLock()
	fn := b.listener
	b.listenerMu.RUnlock()
	if fn != nil {
		fn(event, data)
	}
}

func (b *Bot) baseCtx() context.Context {
	b.pairMu.RLock()
	ctx := b.runCtx
	b.pairMu.RUnlock()
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

func (b *Bot) currentTime() time.Time {
	if b != nil && b.now != nil {
		return b.now().UTC()
	}
	return time.Now().UTC()
}

func (b *Bot) markConnecting() {
	now := b.currentTime()
	b.syncMu.Lock()
	b.acceptAfter = now
	b.offlineSyncing = false
	b.offlineMessagesRemaining = 0
	b.awaitingConnect = true
	b.syncMu.Unlock()
}

func (b *Bot) markConnected() {
	now := b.currentTime()
	b.syncMu.Lock()
	if b.awaitingConnect {
		if now.After(b.acceptAfter) {
			b.acceptAfter = now
		}
		b.awaitingConnect = false
	}
	b.syncMu.Unlock()
}

func (b *Bot) markDisconnected() {
	b.syncMu.Lock()
	b.offlineSyncing = false
	b.offlineMessagesRemaining = 0
	b.awaitingConnect = true
	b.syncMu.Unlock()
}

func (b *Bot) markOfflineSyncStarted(messages int) {
	if messages <= 0 {
		return
	}
	now := b.currentTime()
	b.syncMu.Lock()
	b.offlineSyncing = true
	b.offlineMessagesRemaining = messages
	if b.awaitingConnect && now.After(b.acceptAfter) {
		b.acceptAfter = now
	}
	b.syncMu.Unlock()
}

func (b *Bot) markOfflineSyncCompleted() {
	b.syncMu.Lock()
	b.offlineSyncing = false
	b.offlineMessagesRemaining = 0
	b.syncMu.Unlock()
}

func (b *Bot) shouldIgnoreIncomingMessage(evt *events.Message) (string, bool) {
	if ShouldIgnoreMessage(evt) {
		return "ignored_event", true
	}

	now := b.currentTime()

	b.syncMu.Lock()
	offlineSyncing := b.offlineSyncing
	awaitingConnect := b.awaitingConnect
	acceptAfter := b.acceptAfter
	if offlineSyncing {
		isClearlyLive := !evt.Info.Timestamp.IsZero() &&
			!acceptAfter.IsZero() &&
			evt.Info.Timestamp.UTC().After(acceptAfter.Truncate(time.Second).Add(incomingSkewTolerance))
		if !isClearlyLive {
			if b.offlineMessagesRemaining > 0 {
				b.offlineMessagesRemaining--
				if b.offlineMessagesRemaining == 0 {
					b.offlineSyncing = false
				}
			}
			b.syncMu.Unlock()
			return "offline_sync", true
		}
	}
	b.syncMu.Unlock()

	cutoff := acceptAfter
	if awaitingConnect {
		if now.After(cutoff) {
			cutoff = now
		}
	}

	if !cutoff.IsZero() && !evt.Info.Timestamp.IsZero() {
		earliestAccepted := cutoff.Truncate(time.Second).Add(-incomingSkewTolerance)
		if evt.Info.Timestamp.UTC().Before(earliestAccepted) {
			return "before_connect", true
		}
	}

	return "", false
}

func (b *Bot) warmGroupCache(ctx context.Context) {
	if b.client == nil || b.client.Store == nil || b.client.Store.ID == nil {
		return
	}
	if !b.warmingUp.CompareAndSwap(false, true) {
		return
	}
	defer b.warmingUp.Store(false)

	warmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	joined, err := b.client.GetJoinedGroups(warmCtx)
	if err != nil {
		if b.logger != nil {
			b.logger.Debug("group cache pre-warm skipped", "error", err)
		}
		return
	}
	if b.groups != nil {
		for _, g := range joined {
			b.groups.Prime(g)
		}
	}
}

func NewBot(store *sqlstore.Container, processor *application.Processor, workers, queueSize int, logger *slog.Logger, autoApproveRequests bool, blockedNumbers []string) (*Bot, error) {
	device, err := store.GetFirstDevice(context.Background())
	if err != nil {
		return nil, fmt.Errorf("load WhatsApp device: %w", err)
	}

	client := whatsmeow.NewClient(device, waLog.Stdout("whatsmeow", "INFO", false))
	client.EnableAutoReconnect = true
	client.InitialAutoReconnect = true

	now := time.Now().UTC()
	return &Bot{
		client:               client,
		processor:            processor,
		groups:               newGroupServiceCache(client),
		audioCache:           newStickerAudioCache(),
		workers:              workers,
		queue:                make(chan *events.Message, queueSize),
		logger:               logger,
		autoApproveRequests:  autoApproveRequests,
		blockedNumbers:       blockedNumbers,
		qrSnap:               QRSnapshot{State: "disconnected"},
		acceptAfter:          now,
		awaitingConnect:      true,
		now:                  time.Now,
	}, nil
}

func (b *Bot) Run(ctx context.Context) error {
	b.pairMu.Lock()
	b.runCtx = ctx
	b.pairMu.Unlock()

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	for range b.workers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			b.runWorker(workerCtx)
		}()
	}

	// Start auto-approve join requests worker if enabled
	if b.autoApproveRequests && len(b.blockedNumbers) > 0 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			b.runAutoApproveWorker(workerCtx)
		}()
	}

	b.client.AddEventHandler(b.handleEvent(workerCtx))

	if b.client.Store.ID == nil {
		if err := b.startPairingSession(ctx); err != nil {
			return fmt.Errorf("prepare WhatsApp pairing: %w", err)
		}
	} else {
		b.markConnecting()
		if err := b.client.ConnectContext(ctx); err != nil {
			return fmt.Errorf("connect to WhatsApp: %w", err)
		}
	}

	<-ctx.Done()
	b.cancelPairingSession()
	b.client.Disconnect()
	if b.fastStore != nil {
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 10*time.Second)
		b.fastStore.Close(flushCtx)
		flushCancel()
	}
	return nil
}

func (b *Bot) handleEvent(ctx context.Context) whatsmeow.EventHandler {
	return func(raw any) {
		switch evt := raw.(type) {
		case *events.Message:
			if reason, ignore := b.shouldIgnoreIncomingMessage(evt); ignore {
				if evt != nil && b.logger != nil {
					b.logger.Debug("ignoring incoming WhatsApp message",
						"reason", reason,
						"chat", evt.Info.Chat.String(),
						"message_id", evt.Info.ID,
						"timestamp", evt.Info.Timestamp.UTC(),
					)
				}
				return
			}
			if b.logger != nil {
				b.logger.Debug("received WhatsApp message", "chat", evt.Info.Chat.String(), "sender", evt.Info.Sender.String(), "message_id", evt.Info.ID)
			}
			select {
			case b.queue <- evt:
			case <-ctx.Done():
			default:
				if b.logger != nil {
					b.logger.Warn("message queue full; dropping incoming message", "chat", evt.Info.Chat.String(), "message_id", evt.Info.ID)
				}
			}
		case *events.OfflineSyncPreview:
			if evt == nil {
				return
			}
			if b.logger != nil {
				b.logger.Info("WhatsApp offline sync started", "total", evt.Total, "messages", evt.Messages)
			}
			b.markOfflineSyncStarted(evt.Messages)
		case *events.OfflineSyncCompleted:
			if evt == nil {
				return
			}
			if b.logger != nil {
				b.logger.Info("WhatsApp offline sync completed", "count", evt.Count)
			}
			b.markOfflineSyncCompleted()
		case *events.GroupInfo:
			if evt != nil && b.groups != nil {
				b.groups.Invalidate(evt.JID.String())
			}
		case *events.Connected:
			if b.logger != nil {
				b.logger.Info("WhatsApp connected")
			}
			b.markConnected()
			connectedAt := b.currentTime()
			b.pairMu.Lock()
			b.connectedAt = connectedAt
			b.qrSnap = QRSnapshot{State: "connected"}
			b.pairMu.Unlock()
			b.emitEvent("status", map[string]any{"event": "connected"})
			go b.warmGroupCache(ctx)
		case *events.Disconnected:
			if b.logger != nil {
				b.logger.Warn("WhatsApp disconnected")
			}
			b.markDisconnected()
			b.pairMu.Lock()
			b.connectedAt = time.Time{}
			if b.client != nil && b.client.Store != nil && b.client.Store.ID != nil {
				b.qrSnap = QRSnapshot{State: "disconnected"}
			}
			b.pairMu.Unlock()
			b.emitEvent("status", map[string]any{"event": "disconnected"})
		case *events.LoggedOut:
			reason := ""
			if evt != nil {
				reason = evt.Reason.String()
			}
			if b.logger != nil {
				b.logger.Error("WhatsApp session logged out", "reason", reason)
			}
			b.markDisconnected()
			b.pairMu.Lock()
b.connectedAt = time.Time{}
			b.qrSnap = QRSnapshot{State: "disconnected"}
			b.pairMu.Unlock()
			b.emitEvent("status", map[string]any{"event": "logged_out", "reason": reason})
			if ctx.Err() == nil {
				go func() {
					if err := b.startPairingSession(b.baseCtx()); err != nil && b.logger != nil {
						b.logger.Warn("failed to restart pairing session after logout", "error", err)
					}
				}()
			}
		}
	}
}

// runAutoApproveWorker periodically checks for pending join requests in groups
// where the bot is an admin and automatically approves them (except blocked numbers).
func (b *Bot) runAutoApproveWorker(ctx context.Context) {
	if b.logger != nil {
		b.logger.Info("auto-approve join requests worker started")
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if b.logger != nil {
				b.logger.Info("auto-approve join requests worker stopped")
			}
			return
		case <-ticker.C:
			b.processPendingJoinRequests(ctx)
		}
	}
}

func (b *Bot) processPendingJoinRequests(ctx context.Context) {
	if b.groups == nil || b.client == nil || b.client.Store == nil || b.client.Store.ID == nil {
		return
	}

	joinedGroups, err := b.client.GetJoinedGroups(ctx)
	if err != nil {
		if b.logger != nil {
			b.logger.Debug("failed to get joined groups for auto-approve", "error", err)
		}
		return
	}

	for _, group := range joinedGroups {
		groupJID := group.JID.String()

		// Check if bot is admin in this group
		groupInfo, err := b.groups.GetInfo(ctx, groupJID)
		if err != nil {
			continue
		}

		botIsAdmin := false
		for _, p := range groupInfo.Participants {
			if p.JID == b.client.Store.ID.String() && p.IsAdmin {
				botIsAdmin = true
				break
			}
		}

		if !botIsAdmin {
			continue
		}

		// Get pending join requests
		reqs, err := b.groups.GetJoinRequests(ctx, groupJID)
		if err != nil || len(reqs) == 0 {
			continue
		}

		var toApprove []string
		var toReject []string

		for _, req := range reqs {
			userNum := jidUser(req.JID)
			blocked := false
			for _, blockedNum := range b.blockedNumbers {
				if jidUser(blockedNum) == userNum {
					blocked = true
					break
				}
			}

			if blocked {
				toReject = append(toReject, req.JID)
				if b.logger != nil {
					b.logger.Info("auto-rejecting join request from blocked number", "group", groupJID, "user", req.JID)
				}
			} else {
				toApprove = append(toApprove, req.JID)
				if b.logger != nil {
					b.logger.Info("auto-approving join request", "group", groupJID, "user", req.JID)
				}
			}
		}

		if len(toApprove) > 0 {
			if err := b.groups.UpdateJoinRequests(ctx, groupJID, toApprove, true); err != nil {
				if b.logger != nil {
					b.logger.Error("failed to auto-approve join requests", "group", groupJID, "error", err)
				}
			}
		}

		if len(toReject) > 0 {
			if err := b.groups.UpdateJoinRequests(ctx, groupJID, toReject, false); err != nil {
				if b.logger != nil {
					b.logger.Error("failed to auto-reject join requests", "group", groupJID, "error", err)
				}
			}
		}
	}
}

// jidUser extracts the user part from a JID
func jidUser(jid string) string {
	jid = strings.TrimSpace(jid)
	if idx := strings.Index(jid, "@"); idx >= 0 {
		jid = jid[:idx]
	}
	if idx := strings.Index(jid, ":"); idx >= 0 {
		jid = jid[:idx]
	}
	return jid
}
