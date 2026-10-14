package platforms

import (
	"context"
	"fmt"
	"sort"
	"time"
)

func day8(now time.Time) string { return now.In(time.FixedZone("UTC+8", 8*3600)).Format("2006-01-02") }
func (c *Client) scheduleState() any {
	state := c.store.snapshot()
	rows := []map[string]any{}
	ids := []string{}
	for id := range c.adapters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		info := c.adapters[id].Info()
		enabled, configured := state.ProviderAuto[id]
		if !configured {
			enabled = true
		}
		row := map[string]any{"id": id, "name": info.Name, "unit": info.Unit, "auto": info.Auto, "claim": info.Claim, "loginMode": info.Login, "enabled": enabled, "runtime": "go"}
		dates := ""
		for _, a := range state.Accounts {
			if a.Provider != id {
				continue
			}
			if a.Checkin.Error != "" {
				row["error"] = a.Checkin.Error
			}
			if a.Checkin.LastDate > dates {
				dates = a.Checkin.LastDate
			}
		}
		if dates != "" {
			row["lastDate"] = dates
		}
		rows = append(rows, row)
	}
	return map[string]any{"enabled": state.Auto, "providers": rows, "runtime": "go"}
}
func (c *Client) setSchedule(payload map[string]any) (any, error) {
	enabled, ok := payload["enabled"].(bool)
	if !ok {
		return nil, fmt.Errorf("enabled 必须为布尔值")
	}
	provider := str(payload["provider"])
	if provider != "" {
		adapter, err := c.adapter(provider)
		if err != nil {
			return nil, err
		}
		if !adapter.Info().Auto {
			return nil, fmt.Errorf("该平台不支持自动签到")
		}
	}
	err := c.store.update(func(s *State) error {
		if provider == "" {
			s.Auto = enabled
		} else {
			s.ProviderAuto[provider] = enabled
		}
		return nil
	})
	return c.scheduleState(), err
}
func (c *Client) claimOne(ctx context.Context, id string) (Outcome, error) {
	lock := keyedLock(&c.claims, id)
	lock.Lock()
	defer lock.Unlock()
	account, err := c.account(id)
	if err != nil {
		return Outcome{}, err
	}
	var outcome Outcome
	current, claimErr := c.credential(ctx, id, false)
	if claimErr == nil {
		adapter, adapterErr := c.adapter(account.Provider)
		if adapterErr != nil {
			claimErr = adapterErr
		} else {
			outcome, claimErr = adapter.Claim(ctx, current.Credential)
		}
	}
	record := Checkin{LastDate: account.Checkin.LastDate, LastAttempt: c.now().UnixMilli()}
	if claimErr != nil {
		record.Error = claimErr.Error()
	} else {
		record.Outcome = &outcome
		if outcome.CoversToday && (outcome.Kind == "claimed" || outcome.Kind == "already-claimed") {
			record.LastDate = day8(c.now())
		}
	}
	saveErr := c.store.update(func(s *State) error {
		a := accountByID(s, id)
		if a == nil {
			return fmt.Errorf("账号已删除")
		}
		a.Checkin = record
		return nil
	})
	if saveErr != nil {
		return Outcome{}, saveErr
	}
	return outcome, claimErr
}
func (c *Client) claimProvider(ctx context.Context, provider string) (any, error) {
	adapter, err := c.adapter(provider)
	if err != nil {
		return nil, err
	}
	if !adapter.Info().Claim {
		return nil, fmt.Errorf("该平台不支持每日领取")
	}
	results := []any{}
	summary := map[string]any{"claimed": 0, "alreadyClaimed": 0, "inactive": 0, "failed": 0, "coversToday": 0, "totalCredit": float64(0), "totalByUnit": map[string]float64{"credit": 0, "token": 0}}
	for _, a := range c.store.snapshot().Accounts {
		if a.Provider != provider {
			continue
		}
		outcome, err := c.claimOne(ctx, a.ID)
		if err != nil {
			outcome = Outcome{Kind: "failed", Message: err.Error()}
		}
		key := outcome.Kind
		if key == "already-claimed" {
			key = "alreadyClaimed"
		}
		count, ok := summary[key].(int)
		if ok {
			summary[key] = count + 1
		}
		if outcome.CoversToday {
			summary["coversToday"] = summary["coversToday"].(int) + 1
		}
		if outcome.Kind == "claimed" {
			summary["totalCredit"] = summary["totalCredit"].(float64) + outcome.Credit
			unit := outcome.Unit
			if unit == "" {
				unit = "credit"
			}
			summary["totalByUnit"].(map[string]float64)[unit] += outcome.Credit
		}
		results = append(results, map[string]any{"accountId": a.ID, "nickname": a.Nickname, "outcome": outcome})
	}
	return success(map[string]any{"results": results, "summary": summary}), nil
}
func (c *Client) runScheduled() {
	if !c.scheduleMu.TryLock() {
		return
	}
	defer c.scheduleMu.Unlock()
	state := c.store.snapshot()
	c.sessions.Range(func(id, value any) bool {
		session := value.(*loginSession)
		session.mu.Lock()
		expired := session.expires.Before(c.now())
		session.mu.Unlock()
		if expired {
			c.sessions.Delete(id)
		}
		return true
	})
	if !state.Auto {
		return
	}
	for _, account := range state.Accounts {
		if !account.Enabled {
			continue
		}
		if c.ctx.Err() != nil {
			return
		}
		adapter := c.adapters[account.Provider]
		if adapter == nil || !adapter.Info().Auto {
			continue
		}
		if enabled, ok := state.ProviderAuto[account.Provider]; ok && !enabled {
			continue
		}
		if account.Checkin.LastDate == day8(c.now()) || c.now().UnixMilli()-account.Checkin.LastAttempt < int64((30*time.Minute)/time.Millisecond) {
			continue
		}
		ctx, cancel := context.WithTimeout(c.ctx, 2*time.Minute)
		_, err := c.claimOne(ctx, account.ID)
		cancel()
		if err != nil {
			c.logError("每日领取失败 ("+account.Provider+")", err)
		}
	}
}
