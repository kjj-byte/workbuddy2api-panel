package platforms

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeAdapter struct{ claims, refreshes, chats atomic.Int32 }

func (*fakeAdapter) Info() Provider {
	return Provider{"loomy", "Test Loomy", "credit", true, true, "sms"}
}
func (*fakeAdapter) Models(context.Context, Credential) ([]Model, error) {
	return []Model{{ID: "test", Name: "Test model"}}, nil
}
func (*fakeAdapter) Balance(context.Context, Credential) (*Balance, error) {
	return &Balance{Total: 100, Packages: []Package{{Name: "永久积分", Remaining: 100, Active: true}}}, nil
}
func (f *fakeAdapter) Claim(_ context.Context, c Credential) (Outcome, error) {
	f.claims.Add(1)
	if c["fail"] == true {
		return Outcome{}, fmt.Errorf("test claim failure")
	}
	return Outcome{Kind: "claimed", Credit: 5, Unit: "credit", CoversToday: true}, nil
}
func (f *fakeAdapter) Refresh(_ context.Context, c Credential) (Credential, error) {
	f.refreshes.Add(1)
	return Credential{"access_token": "refreshed", "expires_at": time.Now().Add(time.Hour).UnixMilli()}, nil
}
func (f *fakeAdapter) Chat(_ context.Context, _ Credential, body map[string]any, receive func(json.RawMessage) error) error {
	f.chats.Add(1)
	if body["model"] != "test" {
		return fmt.Errorf("model namespace leaked upstream")
	}
	if err := emitChunk(receive, map[string]any{"type": "text-delta", "text": "OK"}); err != nil {
		return err
	}
	return emitChunk(receive, map[string]any{"type": "finish", "reason": map[string]any{"kind": "stop"}})
}
func fakeClient(t *testing.T) (*Client, *fakeAdapter) {
	t.Helper()
	f := &fakeAdapter{}
	c, err := newClient(context.Background(), Config{DataDir: t.TempDir()}, []Adapter{f})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, f
}
func addTestAccount(t *testing.T, c *Client, id string, credential Credential) {
	t.Helper()
	if err := c.saveAccount(Account{ID: id, Provider: "loomy", Nickname: id, Enabled: true, Credential: credential}); err != nil {
		t.Fatal(err)
	}
}

func TestAccountListingNeverExposesCredentials(t *testing.T) {
	c, _ := fakeClient(t)
	addTestAccount(t, c, "a", Credential{"access_token": "DO-NOT-EXPOSE"})
	raw, err := c.RPC(context.Background(), "account.list", map[string]any{"provider": "loomy"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "DO-NOT-EXPOSE") || strings.Contains(string(raw), "credential") {
		t.Fatalf("secret in account response: %s", raw)
	}
}
func TestSchedulerTracksEachAccountAndRetriesOnlyFailures(t *testing.T) {
	c, f := fakeClient(t)
	clock := time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	addTestAccount(t, c, "good", Credential{"access_token": "a"})
	addTestAccount(t, c, "bad", Credential{"access_token": "b", "fail": true})
	c.runScheduled()
	if f.claims.Load() != 2 {
		t.Fatal("missing initial claims")
	}
	good, _ := c.account("good")
	bad, _ := c.account("bad")
	if good.Checkin.LastDate != "2026-10-14" || bad.Checkin.LastDate != "" || bad.Checkin.Error == "" {
		t.Fatalf("incorrect independent records %+v %+v", good.Checkin, bad.Checkin)
	}
	c.runScheduled()
	if f.claims.Load() != 2 {
		t.Fatal("retry ignored cooldown")
	}
	clock = clock.Add(31 * time.Minute)
	bad.Credential["fail"] = false
	if err := c.saveAccount(bad); err != nil {
		t.Fatal(err)
	}
	c.runScheduled()
	if f.claims.Load() != 3 {
		t.Fatal("successful account was re-claimed")
	}
	clock = clock.Add(24 * time.Hour)
	c.runScheduled()
	if f.claims.Load() != 5 {
		t.Fatal("no automatic next-day claims")
	}
}
func TestCheckinStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	f := &fakeAdapter{}
	cfg := Config{DataDir: dir}
	c, err := newClient(context.Background(), cfg, []Adapter{f})
	if err != nil {
		t.Fatal(err)
	}
	addTestAccount(t, c, "a", Credential{"access_token": "x"})
	c.runScheduled()
	c.Close()
	restarted, err := newClient(context.Background(), cfg, []Adapter{f})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.runScheduled()
	if f.claims.Load() != 1 {
		t.Fatal("persisted claim was lost")
	}
}
func TestProtectedPermanentCreditsNeverReachChat(t *testing.T) {
	c, f := fakeClient(t)
	addTestAccount(t, c, "a", Credential{"access_token": "x"})
	if _, err := c.rpc(context.Background(), "credits.permanentLock", map[string]any{"provider": "loomy", "locked": true}); err != nil {
		t.Fatal(err)
	}
	err := c.complete(context.Background(), "loomy/test", map[string]any{}, func(json.RawMessage) error { return nil })
	if err == nil || f.chats.Load() != 0 {
		t.Fatal("permanent-credit lock bypassed")
	}
}
func TestDisabledAccountOrModelNeverReachesChat(t *testing.T) {
	c, f := fakeClient(t)
	addTestAccount(t, c, "a", Credential{"access_token": "x"})
	if _, err := c.rpc(context.Background(), "model.setDisabled", map[string]any{"provider": "loomy", "modelId": "test", "disabled": true}); err != nil {
		t.Fatal(err)
	}
	if err := c.complete(context.Background(), "loomy/test", map[string]any{}, func(json.RawMessage) error { return nil }); err == nil || f.chats.Load() != 0 {
		t.Fatal("model disable bypassed")
	}
}
func TestLegacyBackupImportIsAtomicAndSkipsUnsupportedProviders(t *testing.T) {
	c, _ := fakeClient(t)
	backup := map[string]any{"format": "dsh-codearts-auth/backup", "version": 1, "accounts": []any{map[string]any{"id": "old-loomy", "provider": "loomy", "nickname": "old", "enabled": true, "credentialRef": "LOOMY_A"}, map[string]any{"id": "q", "provider": "qoder", "credentialRef": "Q_A"}}, "credentials": map[string]any{"LOOMY_A": `{"access_token":"private"}`}, "disabledModels": map[string]any{"loomy": map[string]any{"hidden": true}}, "permanentLocks": map[string]any{"loomy": true}}
	if _, err := c.importBackup(backup); err != nil {
		t.Fatal(err)
	}
	state := c.store.snapshot()
	if len(state.Accounts) != 1 || state.Accounts[0].ID != "old-loomy" || !state.Disabled["loomy"]["hidden"] || !state.Locks["loomy"] {
		t.Fatalf("wrong migrated state %+v", state)
	}
	backup["credentials"] = map[string]any{"LOOMY_A": "invalid"}
	if _, err := c.importBackup(backup); err == nil {
		t.Fatal("invalid credential accepted")
	}
	if str(c.store.snapshot().Accounts[0].Credential["access_token"]) != "private" {
		t.Fatal("failed import modified existing credentials")
	}
}
func TestRefreshIsSerializedAndPersisted(t *testing.T) {
	c, f := fakeClient(t)
	addTestAccount(t, c, "a", Credential{"access_token": "expired", "expires_at": 1})
	for i := 0; i < 2; i++ {
		a, err := c.credential(context.Background(), "a", false)
		if err != nil || str(a.Credential["access_token"]) != "refreshed" {
			t.Fatal(a, err)
		}
	}
	if f.refreshes.Load() != 1 {
		t.Fatal("refresh repeated despite fresh token")
	}
}
