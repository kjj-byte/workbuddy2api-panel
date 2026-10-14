package platforms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

type loginSession struct {
	mu                                sync.Mutex
	provider, accountID, phone, msgID string
	expires                           time.Time
	done                              bool
	err                               string
}
type Client struct {
	store       *store
	adapters    map[string]Adapter
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	closeOnce   sync.Once
	sessions    sync.Map
	credentials sync.Map
	claims      sync.Map
	scheduleMu  sync.Mutex
	now         func() time.Time
}

func Start(ctx context.Context, cfg Config) (*Client, error) {
	return newClient(ctx, cfg, []Adapter{NewLoomy(), NewMiniMax(), NewLobsterAI(), NewCodeArts(), NewQoder(false), NewQoder(true), NewTrae()})
}
func newClient(ctx context.Context, cfg Config, adapters []Adapter) (*Client, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = "data/platforms-go"
	}
	s, err := openStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	running, cancel := context.WithCancel(ctx)
	c := &Client{store: s, adapters: map[string]Adapter{}, ctx: running, cancel: cancel, done: make(chan struct{}), now: time.Now}
	for _, a := range adapters {
		info := a.Info()
		if _, exists := c.adapters[info.ID]; exists {
			cancel()
			return nil, fmt.Errorf("duplicate provider %s", info.ID)
		}
		c.adapters[info.ID] = a
	}
	go func() {
		defer close(c.done)
		startup := time.NewTimer(30 * time.Second)
		defer startup.Stop()
		select {
		case <-running.Done():
			return
		case <-startup.C:
		}
		c.runScheduled()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-running.Done():
				return
			case <-ticker.C:
				c.runScheduled()
			}
		}
	}()
	return c, nil
}
func (c *Client) Close() { c.closeOnce.Do(func() { c.cancel(); <-c.done }) }
func (c *Client) adapter(id string) (Adapter, error) {
	a := c.adapters[id]
	if a == nil {
		return nil, &RemoteError{"该平台尚未迁移到 Go，本版本不提供此能力", 501}
	}
	return a, nil
}
func (c *Client) account(id string) (Account, error) {
	state := c.store.snapshot()
	account := accountByID(&state, id)
	if account == nil {
		return Account{}, fmt.Errorf("账号不存在")
	}
	return *account, nil
}
func (c *Client) saveAccount(account Account) error {
	return c.store.update(func(state *State) error {
		old := accountByID(state, account.ID)
		if old != nil {
			*old = account
		} else {
			state.Accounts = append(state.Accounts, account)
		}
		return nil
	})
}
func keyedLock(table *sync.Map, id string) *sync.Mutex {
	value, _ := table.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}
func (c *Client) credential(ctx context.Context, id string, force bool) (Account, error) {
	lock := keyedLock(&c.credentials, id)
	lock.Lock()
	defer lock.Unlock()
	account, err := c.account(id)
	if err != nil {
		return Account{}, err
	}
	if err = validatePlatformCredential(account.Provider, account.Credential); err != nil {
		return Account{}, err
	}
	expires, hasExpiry := number(account.Credential["expires_at"])
	if !hasExpiry {
		expires, hasExpiry = number(account.Credential["expire_time"])
	}
	if !hasExpiry {
		if parsed, err := time.Parse(time.RFC3339, str(account.Credential["expires_at"])); err == nil {
			expires = float64(parsed.UnixMilli())
			hasExpiry = true
		}
	}
	if hasExpiry && expires < 1e12 {
		expires *= 1000
	}
	if !force && (!hasExpiry || expires > float64(c.now().Add(time.Minute).UnixMilli())) {
		return account, nil
	}
	adapter, err := c.adapter(account.Provider)
	if err != nil {
		return Account{}, err
	}
	credential, err := adapter.Refresh(ctx, account.Credential)
	if err != nil {
		return Account{}, err
	}
	if err = validatePlatformCredential(account.Provider, credential); err != nil {
		return Account{}, err
	}
	// Update only credentials; a concurrent toggle or check-in must not be overwritten.
	err = c.store.update(func(state *State) error {
		entry := accountByID(state, id)
		if entry == nil {
			return fmt.Errorf("账号已删除")
		}
		entry.Credential = credential
		account = *entry
		return nil
	})
	return account, err
}
func (c *Client) Models(ctx context.Context) ([]map[string]any, error) {
	state := c.store.snapshot()
	providers := make([]string, 0, len(c.adapters))
	for id := range c.adapters {
		providers = append(providers, id)
	}
	sort.Strings(providers)
	rows := []map[string]any{}
	for _, provider := range providers {
		var models []Model
		for _, account := range state.Accounts {
			if account.Provider != provider || !account.Enabled {
				continue
			}
			current, err := c.credential(ctx, account.ID, false)
			if err != nil {
				continue
			}
			models, err = c.adapters[provider].Models(ctx, current.Credential)
			if err == nil {
				break
			}
		}
		seen := map[string]bool{}
		for _, m := range models {
			if seen[m.ID] || state.Disabled[provider][m.ID] {
				continue
			}
			seen[m.ID] = true
			row := map[string]any{"id": provider + "/" + m.ID, "object": "model", "owned_by": provider, "name": m.Name, "vision": m.Vision}
			if m.ContextWindow > 0 {
				row["context_window"] = m.ContextWindow
			}
			if m.MaxTokens > 0 {
				row["max_tokens"] = m.MaxTokens
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}
func (c *Client) Call(ctx context.Context, operation map[string]any, onChunk func(json.RawMessage) error) (json.RawMessage, error) {
	if c.ctx.Err() != nil {
		return nil, fmt.Errorf("平台服务已停止")
	}
	combined, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	defer cancel()
	ctx = combined
	var value any
	var err error
	switch operation["type"] {
	case "models":
		value, err = c.Models(ctx)
	case "scheduleState":
		value = c.scheduleState()
	case "scheduleSet":
		value, err = c.setSchedule(obj(operation["payload"]))
	case "checkin":
		value, err = c.claimProvider(ctx, str(operation["provider"]))
	case "rpc":
		var call struct {
			Method  string         `json:"method"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal(jsonBytes(operation["call"]), &call) != nil {
			return nil, fmt.Errorf("invalid RPC")
		}
		value, err = c.rpc(ctx, call.Method, call.Payload)
	case "complete":
		request := obj(operation["request"])
		body := obj(request["openAi"])
		if onChunk == nil {
			return nil, fmt.Errorf("stream callback missing")
		}
		err = c.complete(ctx, str(request["model"]), body, onChunk)
	default:
		err = fmt.Errorf("unsupported platform operation")
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(jsonBytes(value)), nil
}
func (c *Client) RPC(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	return c.Call(ctx, map[string]any{"type": "rpc", "call": map[string]any{"method": method, "payload": payload}}, nil)
}
func success(value any) map[string]any { return map[string]any{"ok": true, "value": value} }
func (c *Client) complete(ctx context.Context, model string, body map[string]any, receive func(json.RawMessage) error) error {
	provider, bare, qualified := strings.Cut(model, "/")
	if !qualified || bare == "" {
		return &RemoteError{"模型 ID 必须为 provider/model", 400}
	}
	adapter, err := c.adapter(provider)
	if err != nil {
		return err
	}
	state := c.store.snapshot()
	if state.Disabled[provider][bare] {
		return &RemoteError{"模型已停用", 404}
	}
	candidates := []Account{}
	for _, a := range state.Accounts {
		if a.Provider == provider && a.Enabled {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		return &RemoteError{"该平台没有启用的账号", 503}
	}
	// Preserve account priority and enforce the daily-pool guard before chat.
	var last error
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		account, err := c.credential(ctx, candidate.ID, false)
		if err != nil {
			last = err
			continue
		}
		if state.Locks[provider] {
			balance, err := adapter.Balance(ctx, account.Credential)
			if err != nil {
				last = err
				continue
			}
			allowed := false
			for _, p := range balance.Packages {
				if p.Name == "每日赠送" && p.Remaining > 0 {
					allowed = true
				}
			}
			if !allowed {
				last = fmt.Errorf("永久积分保护已开启，没有可用的每日额度")
				continue
			}
		}
		request := map[string]any{}
		if err = json.Unmarshal(jsonBytes(body), &request); err != nil {
			return err
		}
		request["model"] = bare
		emitted := false
		err = adapter.Chat(ctx, account.Credential, request, func(chunk json.RawMessage) error { emitted = true; return receive(chunk) })
		var rejected *RemoteError
		if !emitted && errors.As(err, &rejected) && rejected.Status == 401 {
			if refreshed, refreshErr := c.credential(ctx, candidate.ID, true); refreshErr == nil {
				err = adapter.Chat(ctx, refreshed.Credential, request, func(chunk json.RawMessage) error { emitted = true; return receive(chunk) })
			}
		}
		if err == nil {
			return nil
		}
		last = err
		if emitted {
			return err
		}
		var remote *RemoteError
		if !errors.As(err, &remote) || !(remote.Status == 401 || remote.Status == 402 || remote.Status == 429 || remote.Status >= 500) {
			return err
		}
	}
	if last == nil {
		last = fmt.Errorf("没有可用账号")
	}
	return last
}
func (c *Client) rpc(ctx context.Context, method string, payload map[string]any) (any, error) {
	provider := str(payload["provider"])
	id := str(payload["accountId"])
	switch method {
	case "account.list":
		accounts := []Account{}
		for _, a := range c.store.snapshot().Accounts {
			if a.Provider == provider {
				accounts = append(accounts, a.public())
			}
		}
		return success(map[string]any{"accounts": accounts}), nil
	case "account.create":
		return c.beginLogin(ctx, provider)
	case "login.poll":
		session, ok := c.sessions.Load(id)
		if !ok {
			return success(map[string]any{"done": true, "success": false, "error": "登录会话已过期"}), nil
		}
		s := session.(*loginSession)
		s.mu.Lock()
		defer s.mu.Unlock()
		return success(map[string]any{"done": s.done, "success": s.done && s.err == "", "error": s.err}), nil
	case "login.sendSms", "login.submitSms":
		return c.sms(ctx, method, id, payload)
	case "account.update":
		err := c.store.update(func(s *State) error {
			a := accountByID(s, id)
			if a == nil {
				return fmt.Errorf("账号不存在")
			}
			patch := obj(payload["patch"])
			if n, ok := patch["nickname"].(string); ok {
				a.Nickname = n
			}
			if enabled, ok := patch["enabled"].(bool); ok {
				a.Enabled = enabled
			}
			return nil
		})
		return success(map[string]any{}), err
	case "account.delete":
		err := c.store.update(func(s *State) error {
			for i, a := range s.Accounts {
				if a.ID == id {
					s.Accounts = append(s.Accounts[:i], s.Accounts[i+1:]...)
					return nil
				}
			}
			return fmt.Errorf("账号不存在")
		})
		c.sessions.Delete(id)
		return success(map[string]any{}), err
	case "account.refresh":
		_, err := c.credential(ctx, id, true)
		return success(map[string]any{}), err
	case "account.import":
		credential := Credential(obj(payload["credential"]))
		if _, err := c.adapter(provider); err != nil {
			return nil, err
		}
		if err := validatePlatformCredential(provider, credential); err != nil {
			return nil, err
		}
		a := Account{ID: provider + "-" + identifier(), Provider: provider, Nickname: str(payload["nickname"]), Enabled: true, CreatedAt: c.now().UnixMilli(), Credential: credential}
		if a.Nickname == "" {
			a.Nickname = a.ID
		}
		err := c.saveAccount(a)
		return success(map[string]any{"accountId": a.ID}), err
	case "credits.balances":
		adapter, err := c.adapter(provider)
		if err != nil {
			return nil, err
		}
		rows := []any{}
		for _, a := range c.store.snapshot().Accounts {
			if a.Provider != provider {
				continue
			}
			current, err := c.credential(ctx, a.ID, false)
			var balance *Balance
			if err == nil {
				balance, err = adapter.Balance(ctx, current.Credential)
			}
			row := map[string]any{"accountId": a.ID, "nickname": a.Nickname, "balance": balance}
			if err != nil {
				row["error"] = err.Error()
			}
			rows = append(rows, row)
		}
		return success(map[string]any{"accounts": rows}), nil
	case "credits.permanentLock":
		if provider != "loomy" {
			return nil, fmt.Errorf("该平台未提供永久积分保护")
		}
		if locked, ok := payload["locked"]; ok {
			b, valid := locked.(bool)
			if !valid {
				return nil, fmt.Errorf("locked 必须为布尔值")
			}
			if err := c.store.update(func(s *State) error { s.Locks[provider] = b; return nil }); err != nil {
				return nil, err
			}
		}
		return success(map[string]any{"locked": c.store.snapshot().Locks[provider]}), nil
	case "model.list":
		adapter, err := c.adapter(provider)
		if err != nil {
			return nil, err
		}
		state := c.store.snapshot()
		catalog := map[string]Model{}
		for _, account := range state.Accounts {
			if account.Provider != provider {
				continue
			}
			current, err := c.credential(ctx, account.ID, false)
			if err != nil {
				continue
			}
			models, err := adapter.Models(ctx, current.Credential)
			if err != nil {
				continue
			}
			for _, m := range models {
				catalog[m.ID] = m
			}
			break
		}
		for model := range state.Disabled[provider] {
			if _, ok := catalog[model]; !ok {
				catalog[model] = Model{ID: model, Name: model}
			}
		}
		models := []any{}
		keys := []string{}
		for key := range catalog {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			m := catalog[key]
			models = append(models, map[string]any{"id": m.ID, "name": m.Name, "disabled": state.Disabled[provider][m.ID]})
		}
		return success(map[string]any{"models": models}), nil
	case "model.setDisabled":
		model := str(payload["modelId"])
		disabled, valid := payload["disabled"].(bool)
		if model == "" || !valid {
			return nil, fmt.Errorf("modelId 和 disabled 必填")
		}
		if _, err := c.adapter(provider); err != nil {
			return nil, err
		}
		err := c.store.update(func(s *State) error {
			if s.Disabled[provider] == nil {
				s.Disabled[provider] = map[string]bool{}
			}
			s.Disabled[provider][model] = disabled
			return nil
		})
		return success(map[string]any{}), err
	case "backup.export":
		return success(map[string]any{"payload": map[string]any{"schema": "wb2api/native-platforms/v1", "state": c.store.snapshot()}}), nil
	case "backup.import":
		return c.importBackup(payload["payload"])
	default:
		return nil, fmt.Errorf("此原生 Go 版本不支持该操作: %s", method)
	}
}
func (c *Client) importBackup(raw any) (any, error) {
	backup := obj(raw)
	if str(backup["schema"]) != "wb2api/native-platforms/v1" {
		return c.importLegacyBackup(backup)
	}
	var incoming State
	if json.Unmarshal(jsonBytes(backup["state"]), &incoming) != nil || incoming.Version != 1 {
		return nil, fmt.Errorf("备份格式无效")
	}
	ids := map[string]bool{}
	for _, a := range incoming.Accounts {
		if _, err := c.adapter(a.Provider); err != nil {
			return nil, err
		}
		if a.ID == "" || ids[a.ID] {
			return nil, fmt.Errorf("备份账号 ID 为空或重复")
		}
		ids[a.ID] = true
		if err := validatePlatformCredential(a.Provider, a.Credential); err != nil {
			return nil, err
		}
	}
	err := c.store.update(func(state *State) error {
		for _, a := range incoming.Accounts {
			if old := accountByID(state, a.ID); old != nil {
				*old = a
			} else {
				state.Accounts = append(state.Accounts, a)
			}
		}
		for provider, models := range incoming.Disabled {
			if c.adapters[provider] != nil {
				state.Disabled[provider] = models
			}
		}
		for provider, locked := range incoming.Locks {
			if c.adapters[provider] != nil {
				state.Locks[provider] = locked
			}
		}
		return nil
	})
	return success(map[string]any{"imported": len(incoming.Accounts)}), err
}
func (c *Client) beginLogin(ctx context.Context, provider string) (any, error) {
	adapter, err := c.adapter(provider)
	if err != nil {
		return nil, err
	}
	id := provider + "-" + identifier()
	session := &loginSession{provider: provider, accountID: id, expires: c.now().Add(15 * time.Minute)}
	if _, ok := adapter.(SMSAdapter); ok {
		c.sessions.Store(id, session)
		return success(map[string]any{"accountId": id, "loginMode": "sms"}), nil
	}
	if browser, ok := adapter.(BrowserAdapter); ok {
		loginCtx, cancel := context.WithDeadline(c.ctx, session.expires)
		auth, err := browser.BeginBrowserLogin(loginCtx)
		if err != nil {
			cancel()
			return nil, err
		}
		c.sessions.Store(id, session)
		go func() {
			defer cancel()
			var result LoginResult
			select {
			case result = <-auth.Result:
			case <-loginCtx.Done():
				result.Err = loginCtx.Err()
			}
			if result.Err == nil {
				result.Err = validatePlatformCredential(provider, result.Credential)
			}
			if result.Err == nil && loginCtx.Err() != nil {
				result.Err = loginCtx.Err()
			}
			if result.Err == nil {
				nickname := str(result.Credential["nickname"])
				if nickname == "" {
					nickname = str(result.Credential["user_name"])
				}
				if nickname == "" {
					nickname = provider + " " + id[len(id)-6:]
				}
				result.Err = c.saveAccount(Account{ID: id, Provider: provider, Nickname: nickname, Enabled: true, CreatedAt: c.now().UnixMilli(), Credential: result.Credential})
			}
			session.mu.Lock()
			session.done = true
			if result.Err != nil {
				session.err = result.Err.Error()
			}
			session.mu.Unlock()
		}()
		return success(map[string]any{"accountId": id, "loginUrl": auth.URL, "loginMode": "browser"}), nil
	}
	device, ok := adapter.(DeviceAdapter)
	if !ok {
		return nil, fmt.Errorf("该平台只支持导入凭据")
	}
	auth, err := device.BeginLogin(ctx)
	if err != nil {
		return nil, err
	}
	session.expires = c.now().Add(auth.Expires)
	c.sessions.Store(id, session)
	go func() {
		loginCtx, cancel := context.WithDeadline(c.ctx, session.expires)
		defer cancel()
		ticker := time.NewTicker(auth.Interval)
		defer ticker.Stop()
		finish := func(message string) {
			session.mu.Lock()
			session.done = true
			session.err = message
			session.mu.Unlock()
		}
		for {
			select {
			case <-loginCtx.Done():
				finish("登录已超时或服务已停止")
				return
			case <-ticker.C:
				credential, done, err := device.PollLogin(loginCtx, auth)
				if err != nil {
					finish(err.Error())
					return
				}
				if !done {
					continue
				}
				err = c.saveAccount(Account{ID: id, Provider: provider, Nickname: provider + " " + id[len(id)-6:], Enabled: true, CreatedAt: c.now().UnixMilli(), Credential: credential})
				if err != nil {
					finish(err.Error())
				} else {
					finish("")
				}
				return
			}
		}
	}()
	return success(map[string]any{"accountId": id, "loginUrl": auth.URL, "loginMode": "device"}), nil
}
func (c *Client) sms(ctx context.Context, method, id string, payload map[string]any) (any, error) {
	value, ok := c.sessions.Load(id)
	if !ok {
		return nil, fmt.Errorf("短信登录会话已过期")
	}
	session := value.(*loginSession)
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.expires.Before(c.now()) {
		return nil, fmt.Errorf("短信登录会话已过期")
	}
	adapter, ok := c.adapters[session.provider].(SMSAdapter)
	if !ok {
		return nil, fmt.Errorf("不支持短信登录")
	}
	if method == "login.sendSms" {
		phone := str(payload["phone"])
		if len(phone) != 11 || phone[0] != '1' || strings.Trim(phone, "0123456789") != "" {
			return nil, fmt.Errorf("请输入 11 位手机号")
		}
		msg, err := adapter.SendSMS(ctx, phone)
		if err != nil {
			return nil, err
		}
		session.phone = phone
		session.msgID = msg
		return success(map[string]any{"sent": true}), nil
	}
	if session.msgID == "" {
		return nil, fmt.Errorf("请先发送验证码")
	}
	credential, err := adapter.LoginSMS(ctx, session.phone, str(payload["code"]), session.msgID)
	if err != nil {
		return nil, err
	}
	nickname := "Loomy " + session.phone[len(session.phone)-4:]
	err = c.saveAccount(Account{ID: id, Provider: session.provider, Nickname: nickname, Enabled: true, CreatedAt: c.now().UnixMilli(), Credential: credential})
	if err != nil {
		return nil, err
	}
	session.done = true
	session.msgID = ""
	return success(map[string]any{"done": true}), nil
}
func (c *Client) logError(message string, err error) {
	if !errors.Is(err, context.Canceled) {
		log.Printf("[platforms] %s: %v", message, err)
	}
}
