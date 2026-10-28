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
)

// fakeAdminPanel implements BotPanel with fixed data.
type fakeAdminPanel struct {
	status      BotStatus
	users       []BotUser
	nodes       []BotNode
	plans       []BotPlan
	statusCalls int
}

func (f *fakeAdminPanel) StatusSummary(context.Context) (BotStatus, error) {
	f.statusCalls++
	return f.status, nil
}
func (f *fakeAdminPanel) TopUsers(context.Context, int) ([]BotUser, error)      { return f.users, nil }
func (f *fakeAdminPanel) FindUser(context.Context, string) (*BotUser, error)    { return nil, io.EOF }
func (f *fakeAdminPanel) SetUserStatus(context.Context, string, string) error   { return nil }
func (f *fakeAdminPanel) OnlineCount(context.Context) (int, error)              { return 3, nil }
func (f *fakeAdminPanel) NodesSummary(context.Context) ([]BotNode, error)       { return f.nodes, nil }
func (f *fakeAdminPanel) ListPlans(context.Context) ([]BotPlan, error)          { return f.plans, nil }
func (f *fakeAdminPanel) PurchasePlan(context.Context, string, string) (string, error) {
	return "ok", nil
}

type adminTG struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (a *adminTG) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.mu.Lock()
			a.sent = append(a.sent, body)
			a.mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (a *adminTG) lastText(t *testing.T) string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sent) == 0 {
		t.Fatal("admin bot sent nothing")
	}
	text, _ := a.sent[len(a.sent)-1]["text"].(string)
	return text
}

func (a *adminTG) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sent)
}

// TestAdminBotAnswersOnlyItsChat proves the admin bot is bound to one chat:
// anyone else talking to the bot gets silence, not a status dump.
func TestAdminBotAnswersOnlyItsChat(t *testing.T) {
	panel := &fakeAdminPanel{status: BotStatus{TotalUsers: 12, ActiveUsers: 9, TotalNodes: 2, OnlineNodes: 2, Uptime: "3d"}}
	tg := &adminTG{}
	srv := tg.server(t)
	bot := NewTelegramBot("TESTTOKEN", 555, panel, slog.New(slog.NewTextHandler(io.Discard, nil)))
	bot.apiBase = srv.URL
	ctx := context.Background()

	bot.handleUpdate(ctx, update(999, "/status"))
	if tg.count() != 0 {
		t.Fatal("must not answer an unauthorised chat")
	}

	bot.handleUpdate(ctx, update(555, "/status"))
	if got := tg.lastText(t); !strings.Contains(got, "12") {
		t.Fatalf("/status reply missing figures: %q", got)
	}

	// A whitespace-only message used to index an empty slice and panic the loop.
	bot.handleUpdate(ctx, update(555, "   "))
	if got := tg.count(); got != 1 {
		t.Fatalf("whitespace message must be ignored, sent %d messages", got)
	}

	bot.handleUpdate(ctx, update(555, "/help"))
	if help := tg.lastText(t); !strings.Contains(help, "/status") {
		t.Fatalf("/help reply looks wrong: %q", help)
	}
}
