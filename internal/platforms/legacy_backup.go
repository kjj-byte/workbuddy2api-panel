package platforms

import (
	"encoding/json"
	"fmt"
)

// Import the public account-backup format only; no DSH directories are read.
func (c *Client) importLegacyBackup(backup map[string]any) (any, error) {
	version, _ := number(backup["version"])
	if str(backup["format"]) != "dsh-codearts-auth/backup" || version != 1 {
		return nil, fmt.Errorf("不是受支持的账号备份格式")
	}
	credentials, valid := backup["credentials"].(map[string]any)
	if !valid {
		return nil, fmt.Errorf("备份缺少凭据字典")
	}
	accounts := []Account{}
	skipped := []string{}
	ids := map[string]bool{}
	for _, entry := range list(backup["accounts"]) {
		a := obj(entry)
		provider := str(a["provider"])
		if c.adapters[provider] == nil {
			skipped = append(skipped, provider)
			continue
		}
		id := str(a["id"])
		if id == "" || ids[id] {
			return nil, fmt.Errorf("备份账号 ID 为空或重复")
		}
		ids[id] = true
		raw := str(credentials[str(a["credentialRef"])])
		var credential Credential
		if json.Unmarshal([]byte(raw), &credential) != nil {
			return nil, fmt.Errorf("备份中的账号凭据无效")
		}
		if err := validatePlatformCredential(provider, credential); err != nil {
			return nil, err
		}
		created, _ := number(a["createdAt"])
		enabled := a["enabled"] != false
		accounts = append(accounts, Account{ID: id, Provider: provider, Nickname: str(a["nickname"]), Enabled: enabled, CreatedAt: int64(created), Credential: credential})
	}
	if len(accounts) == 0 {
		return nil, fmt.Errorf("备份没有当前 Go 版本支持的平台账号")
	}
	var disabled map[string]map[string]bool
	_ = json.Unmarshal(jsonBytes(backup["disabledModels"]), &disabled)
	locks := obj(backup["permanentLocks"])
	err := c.store.update(func(state *State) error {
		for _, a := range accounts {
			if old := accountByID(state, a.ID); old != nil {
				*old = a
			} else {
				state.Accounts = append(state.Accounts, a)
			}
		}
		for provider, models := range disabled {
			if c.adapters[provider] != nil {
				state.Disabled[provider] = models
			}
		}
		for provider, locked := range locks {
			if c.adapters[provider] != nil {
				if b, ok := locked.(bool); ok {
					state.Locks[provider] = b
				}
			}
		}
		return nil
	})
	return success(map[string]any{"imported": len(accounts), "skippedProviders": skipped}), err
}
