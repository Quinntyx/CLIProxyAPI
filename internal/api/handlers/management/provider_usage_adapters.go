package management

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// usageAdapter reads one credential's subscription usage. Requests only go to
// the provider's own configured base URL with that provider's credential.
type usageAdapter func(ctx context.Context, h *Handler, provider *configuredProvider, auth *coreauth.Auth, now time.Time) ProviderUsageAccount

const maxUsageResponseBytes = 1 << 20

// usageAdapter selects an implemented adapter. OpenAI-compatible providers are
// matched by their base-url host, or by provider id for proxied endpoints.
func (p *configuredProvider) usageAdapter() usageAdapter {
	if p.compat == nil {
		if coreauth.ProviderSupportsQuotaWindows(p.providerKey) {
			return nativeQuotaUsage
		}
		return nil
	}
	host, path := "", ""
	if base := providerBaseURL(p, nil); base != nil {
		host, path = strings.ToLower(base.Hostname()), base.Path
	}
	switch {
	case host == "api.mistral.ai" || p.id == "mistral":
		return mistralUsage
	case (host == "opencode.ai" && strings.HasPrefix(path, "/zen/go")) || p.id == "opencode-go":
		return opencodeGoUsage
	case host == "openrouter.ai" || p.id == "openrouter":
		return openRouterUsage
	default:
		return nil
	}
}

func usageFailure(status, message string) ProviderUsageAccount {
	return ProviderUsageAccount{Status: status, Error: message}
}

// usageGetJSON performs one authenticated GET relative to the provider base URL.
func (h *Handler) usageGetJSON(ctx context.Context, provider *configuredProvider, auth *coreauth.Auth, path string) (gjson.Result, *ProviderUsageAccount) {
	base := providerBaseURL(provider, auth)
	if base == nil {
		failure := usageFailure(usageStatusError, "provider base-url is not a valid http(s) URL")
		return gjson.Result{}, &failure
	}
	apiKey := strings.TrimSpace(auth.Attributes["api_key"])
	if apiKey == "" {
		failure := usageFailure(usageStatusUnavailable, "no API key configured")
		return gjson.Result{}, &failure
	}
	endpoint := strings.TrimSuffix(base.String(), "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		failure := usageFailure(usageStatusError, "invalid usage request")
		return gjson.Result{}, &failure
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: h.apiCallTransport(auth, "")}
	resp, err := client.Do(req)
	if err != nil {
		log.WithError(err).WithField("provider", provider.id).Debug("provider usage request failed")
		failure := usageFailure(usageStatusError, "usage request failed")
		return gjson.Result{}, &failure
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("provider usage response close error: %v", errClose)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageResponseBytes))
	if err != nil {
		failure := usageFailure(usageStatusError, "usage response could not be read")
		return gjson.Result{}, &failure
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := usageFailure(usageStatusError, fmt.Sprintf("usage endpoint returned HTTP %d", resp.StatusCode))
		return gjson.Result{}, &failure
	}
	if !gjson.ValidBytes(body) {
		failure := usageFailure(usageStatusError, "usage endpoint returned invalid JSON")
		return gjson.Result{}, &failure
	}
	return gjson.ParseBytes(body), nil
}

func numberPtr(value gjson.Result) *float64 {
	if value.Type != gjson.Number {
		return nil
	}
	v := value.Float()
	return &v
}

func floatPtr(v float64) *float64 { return &v }

// normalizeResetTime converts RFC3339 strings or Unix seconds/milliseconds to RFC3339.
func normalizeResetTime(value gjson.Result) string {
	var seconds float64
	switch value.Type {
	case gjson.String:
		raw := strings.TrimSpace(value.String())
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return ""
		}
		seconds = n
	case gjson.Number:
		seconds = value.Float()
	default:
		return ""
	}
	if seconds <= 0 {
		return ""
	}
	if seconds > 1e12 {
		seconds /= 1000
	}
	return time.Unix(int64(seconds), 0).UTC().Format(time.RFC3339)
}

func finishUsageAccount(account ProviderUsageAccount) ProviderUsageAccount {
	if len(account.Windows) == 0 {
		account.Status = usageStatusUnavailable
		account.Error = "usage response had no recognized windows"
		return account
	}
	account.Status = usageStatusOK
	return account
}

// mistralUsage reports the Vibe and API budgets separately.
func mistralUsage(ctx context.Context, h *Handler, provider *configuredProvider, auth *coreauth.Auth, _ time.Time) ProviderUsageAccount {
	budget, failure := h.usageGetJSON(ctx, provider, auth, "/billing/budget")
	if failure != nil {
		return *failure
	}
	var account ProviderUsageAccount
	for _, item := range []struct{ field, id, label string }{
		{"vibe_budget", "vibe", "Vibe"},
		{"api_budget", "api", "API"},
	} {
		entry := budget.Get(item.field)
		if !entry.IsObject() {
			continue
		}
		window := ProviderUsageWindow{ID: item.id, Label: item.label}
		window.UsedPercent = numberPtr(entry.Get("usage_percentage"))
		window.Limit = numberPtr(entry.Get("initial_budget"))
		if window.Limit != nil && window.UsedPercent != nil {
			used := *window.Limit * *window.UsedPercent / 100
			window.Used = floatPtr(used)
			window.Remaining = floatPtr(*window.Limit - used)
		}
		window.Unit = strings.ToLower(strings.TrimSpace(entry.Get("currency").String()))
		window.ResetsAt = normalizeResetTime(entry.Get("reset_at"))
		account.Windows = append(account.Windows, window)
	}
	if subscription, errSub := h.usageGetJSON(ctx, provider, auth, "/billing/subscription"); errSub == nil {
		account.Plan = strings.TrimSpace(subscription.Get("plan_name").String())
	}
	return finishUsageAccount(account)
}

// opencodeGoUsage reports the rolling, weekly and monthly windows separately.
func opencodeGoUsage(ctx context.Context, h *Handler, provider *configuredProvider, auth *coreauth.Auth, _ time.Time) ProviderUsageAccount {
	usage, failure := h.usageGetJSON(ctx, provider, auth, "/usage")
	if failure != nil {
		return *failure
	}
	var account ProviderUsageAccount
	for _, item := range []struct{ field, label string }{
		{"rolling", "Rolling"},
		{"weekly", "Weekly"},
		{"monthly", "Monthly"},
	} {
		entry := usage.Get("usage." + item.field)
		if !entry.IsObject() {
			continue
		}
		account.Windows = append(account.Windows, ProviderUsageWindow{
			ID:          item.field,
			Label:       item.label,
			UsedPercent: numberPtr(entry.Get("percent")),
			ResetsAt:    normalizeResetTime(entry.Get("resetsAt")),
		})
	}
	return finishUsageAccount(account)
}

// openRouterUsage reports key spend and, when the key may read them, account
// credits. A null key limit is reported as unknown, never as zero or unlimited.
func openRouterUsage(ctx context.Context, h *Handler, provider *configuredProvider, auth *coreauth.Auth, _ time.Time) ProviderUsageAccount {
	key, failure := h.usageGetJSON(ctx, provider, auth, "/key")
	if failure != nil {
		return *failure
	}
	data := key.Get("data")
	var account ProviderUsageAccount
	if used := numberPtr(data.Get("usage")); used != nil {
		window := ProviderUsageWindow{ID: "key", Label: "Key spend", Used: used, Unit: "usd"}
		window.Limit = numberPtr(data.Get("limit"))
		window.Remaining = numberPtr(data.Get("limit_remaining"))
		if window.Limit != nil && *window.Limit > 0 {
			window.UsedPercent = floatPtr(*used / *window.Limit * 100)
		}
		account.Windows = append(account.Windows, window)
	}
	for _, item := range []struct{ field, id, label string }{
		{"usage_daily", "key-daily", "Key spend today"},
		{"usage_weekly", "key-weekly", "Key spend this week"},
		{"usage_monthly", "key-monthly", "Key spend this month"},
	} {
		if used := numberPtr(data.Get(item.field)); used != nil {
			account.Windows = append(account.Windows, ProviderUsageWindow{ID: item.id, Label: item.label, Used: used, Unit: "usd"})
		}
	}
	if data.Get("is_free_tier").Type == gjson.True {
		account.Plan = "Free tier"
	}
	if credits, errCredits := h.usageGetJSON(ctx, provider, auth, "/credits"); errCredits == nil {
		total := numberPtr(credits.Get("data.total_credits"))
		used := numberPtr(credits.Get("data.total_usage"))
		if total != nil && used != nil {
			window := ProviderUsageWindow{ID: "credits", Label: "Account credits", Used: used, Limit: total, Remaining: floatPtr(*total - *used), Unit: "usd"}
			if *total > 0 {
				window.UsedPercent = floatPtr(*used / *total * 100)
			}
			account.Windows = append(account.Windows, window)
		}
	}
	return finishUsageAccount(account)
}

// nativeQuotaUsage reports passive Codex/Claude subscription observations
// without issuing upstream requests.
func nativeQuotaUsage(_ context.Context, _ *Handler, _ *configuredProvider, auth *coreauth.Auth, now time.Time) ProviderUsageAccount {
	observed, _ := coreauth.ObservedQuotaWindows(auth, now)
	var account ProviderUsageAccount
	for _, window := range observed {
		id, label := "weekly", "Weekly"
		if window.Period < 24*time.Hour {
			id, label = "five-hour", "5-hour"
		}
		account.Windows = append(account.Windows, ProviderUsageWindow{
			ID:          id,
			Label:       label,
			UsedPercent: floatPtr(window.UsedPercent),
			ResetsAt:    window.ResetsAt.UTC().Format(time.RFC3339),
		})
	}
	if len(account.Windows) == 0 {
		return usageFailure(usageStatusUnavailable, "no quota observation yet")
	}
	account.Status = usageStatusOK
	return account
}
