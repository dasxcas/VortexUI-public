package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUserPanel implements UserBotPanel with canned data.
type fakeUserPanel struct {
	user     *BotUser
	authErr  error
	subURL   string
	links    []string
	plans    []BotPlan
	authSeen []string
}

func (f *fakeUserPanel) AuthUser(_ context.Context, token string) (*BotUser, error) {
	f.authSeen = append(f.authSeen, token)
	if f.authErr != nil {
		return nil, f.authErr
	}
	if token != "good-token" {
		return nil, io.ErrUnexpectedEOF
	}
	return f.user, nil
}

func (f *fakeUserPanel) GetUserConfigs(context.Context, string) (string, []string, error) {
	return f.subURL, f.links, nil
}

func (f *fakeUserPanel) GetUserPlans(context.Context) ([]BotPlan, error) { return f.plans, nil }

// fakeTelegram records sendMessage calls and serves a scripted update feed.
type fakeTelegram struct {
	mu       sync.Mutex
	sent     []map[string]any
	updates  []tgUpdate
	served   bool
	getCalls int
}

func (f *fakeTelegram) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/botTESTTOKEN/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.sent = append(f.sent, body)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/botTESTTOKEN/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.getCalls++
		first := !f.served
		f.served = true
		updates := []tgUpdate{}
		if first {
			updates = f.updates
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": updates})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeTelegram) messages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeTelegram) lastText(t *testing.T) string {
	t.Helper()
	msgs := f.messages()
	if len(msgs) == 0 {
		t.Fatal("bot sent nothing")
	}
	text, _ := msgs[len(msgs)-1]["text"].(string)
	return text
}

func newTestBot(t *testing.T, panel *fakeUserPanel) (*TelegramUserBot, *fakeTelegram) {
	t.Helper()
	tg := &fakeTelegram{}
	srv := tg.server(t)
	bot := NewTelegramUserBot("TESTTOKEN", panel, slog.New(slog.NewTextHandler(io.Discard, nil)))
	bot.apiBase = srv.URL
	return bot, tg
}

func update(chatID int64, text string) tgUpdate {
	return tgUpdate{UpdateID: 1, Message: &tgMessage{Chat: tgChat{ID: chatID}, Text: text}}
}

// TestUserBotAuthAndConfigs walks the flow a subscriber actually performs:
// authenticate with the token from their subscription link, then ask for links.
func TestUserBotAuthAndConfigs(t *testing.T) {
	panel := &fakeUserPanel{
		user:   &BotUser{Username: "alice", Status: "active", UsedGB: 12.5, LimitGB: 100, DaysLeft: 21, DeviceCount: 2},
		subURL: "https://panel.example.com/sub/good-token",
		links:  []string{"vless://uuid@1.2.3.4:443?type=ws#node"},
	}
	bot, tg := newTestBot(t, panel)
	ctx := context.Background()

	bot.handle(ctx, update(42, "/start bad-token"))
	if !strings.Contains(tg.lastText(t), "Invalid token") {
		t.Fatalf("bad token must be rejected, got %q", tg.lastText(t))
	}

	bot.handle(ctx, update(42, "/start good-token"))
	if got := tg.lastText(t); !strings.Contains(got, "alice") {
		t.Fatalf("valid token must authenticate, got %q", got)
	}

	bot.handle(ctx, update(42, "/me"))
	me := tg.lastText(t)
	for _, want := range []string{"alice", "12.50", "100.00", "21"} {
		if !strings.Contains(me, want) {
			t.Fatalf("/me missing %q: %s", want, me)
		}
	}

	bot.handle(ctx, update(42, "/config"))
	cfg := tg.lastText(t)
	if !strings.Contains(cfg, "https://panel.example.com/sub/good-token") {
		t.Fatalf("/configs missing subscription URL: %s", cfg)
	}
	if !strings.Contains(cfg, "?format=clash") || !strings.Contains(cfg, "?format=singbox") {
		t.Fatalf("/configs missing format links: %s", cfg)
	}
	if !strings.Contains(cfg, "vless://uuid@1.2.3.4:443") {
		t.Fatalf("/configs missing the share link: %s", cfg)
	}
}

// TestUserBotRequiresAuth proves unauthenticated chats cannot read anything.
func TestUserBotRequiresAuth(t *testing.T) {
	panel := &fakeUserPanel{}
	bot, tg := newTestBot(t, panel)
	ctx := context.Background()

	for _, cmd := range []string{"/me", "/configs"} {
		bot.handle(ctx, update(7, cmd))
		if got := tg.lastText(t); !strings.Contains(got, "Not authenticated") {
			t.Fatalf("%s before auth must be refused, got %q", cmd, got)
		}
	}
	if len(panel.authSeen) != 0 {
		t.Fatal("must not call the panel before authentication")
	}
}

// TestUserBotIgnoresNonText keeps stickers/joins from producing replies.
func TestUserBotIgnoresNonText(t *testing.T) {
	bot, tg := newTestBot(t, &fakeUserPanel{})
	bot.handle(context.Background(), tgUpdate{UpdateID: 3})
	bot.handle(context.Background(), update(9, "   "))
	if n := len(tg.messages()); n != 0 {
		t.Fatalf("expected no replies, got %d", n)
	}
}

// TestUserBotRunLoopDispatch proves the polling loop parses a real update and
// answers it — the part that only breaks with live Telegram otherwise.
func TestUserBotRunLoopDispatch(t *testing.T) {
	panel := &fakeUserPanel{}
	tg := &fakeTelegram{updates: []tgUpdate{update(123, "/help")}}
	srv := tg.server(t)
	bot := NewTelegramUserBot("TESTTOKEN", panel, slog.New(slog.NewTextHandler(io.Discard, nil)))
	bot.apiBase = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bot.Run(ctx)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("bot never replied to the polled update")
		case <-time.After(20 * time.Millisecond):
			if len(tg.messages()) > 0 {
				if got := tg.lastText(t); !strings.Contains(got, "/configs") {
					t.Fatalf("unexpected reply: %q", got)
				}
				return
			}
		}
	}
}

// TestUserBotSurvivesTelegramErrors makes sure a failing API does not kill the
// loop: the bot retries instead of exiting.
func TestUserBotSurvivesTelegramErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	bot := NewTelegramUserBot("TESTTOKEN", &fakeUserPanel{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	bot.apiBase = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { bot.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run must return when the context is cancelled")
	}
}
