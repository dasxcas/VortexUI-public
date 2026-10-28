package service

import (
	"context"
	"strings"
	"time"

	"github.com/vortexui/vortexui/internal/notify"
	"github.com/vortexui/vortexui/internal/panel/port"
	"github.com/vortexui/vortexui/internal/subscription"
)

// UserBotAdapter adapts the panel's user/subscription services to the
// notify.UserBotPanel interface consumed by the end-user Telegram bot. It is
// the user-facing counterpart of BotAdapter (which serves admins).
type UserBotAdapter struct {
	users   port.UserRepository
	subs    *SubscriptionService
	plans   *PlanService
	baseURL string
}

// NewUserBotAdapter builds the adapter. baseURL is the public panel origin
// (e.g. https://panel.example.com) used to render subscription links handed
// out in chat; an empty value yields a path-only link.
func NewUserBotAdapter(users port.UserRepository, subs *SubscriptionService, plans *PlanService, baseURL string) *UserBotAdapter {
	return &UserBotAdapter{
		users:   users,
		subs:    subs,
		plans:   plans,
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

var _ notify.UserBotPanel = (*UserBotAdapter)(nil)

// AuthUser resolves a subscription token to the user it belongs to.
func (a *UserBotAdapter) AuthUser(ctx context.Context, token string) (*notify.BotUser, error) {
	u, err := a.users.GetBySubToken(ctx, token)
	if err != nil {
		return nil, err
	}
	out := &notify.BotUser{
		Username:    u.Username,
		Status:      string(u.Status),
		UsedGB:      bytesToGB(u.UsedTraffic),
		LimitGB:     bytesToGB(u.DataLimit),
		DeviceCount: u.DeviceLimit,
	}
	if u.ExpireAt != nil {
		out.DaysLeft = int(time.Until(*u.ExpireAt).Hours() / 24)
	}
	return out, nil
}

// GetUserConfigs returns the stable subscription URL plus per-proxy share links.
func (a *UserBotAdapter) GetUserConfigs(ctx context.Context, token string) (string, []string, error) {
	res, err := a.subs.Build(ctx, token)
	if err != nil {
		return "", nil, err
	}
	links := make([]string, 0, len(res.Proxies))
	for _, p := range res.Proxies {
		if l := subscription.ShareLink(p); l != "" {
			links = append(links, l)
		}
	}
	return a.baseURL + "/sub/" + token, links, nil
}

// GetUserPlans lists enabled plans a user can buy.
func (a *UserBotAdapter) GetUserPlans(ctx context.Context) ([]notify.BotPlan, error) {
	plans, err := a.plans.ListPlans(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]notify.BotPlan, 0, len(plans))
	for _, p := range plans {
		if p == nil || !p.Enabled {
			continue
		}
		out = append(out, notify.BotPlan{
			Name:       p.Name,
			DataGB:     bytesToGB(p.DataLimit),
			Days:       p.Duration,
			PriceToman: p.PriceToman,
			PriceUSD:   p.PriceUSD,
		})
	}
	return out, nil
}

func bytesToGB(b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(b) / (1 << 30)
}
