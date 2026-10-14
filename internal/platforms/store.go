package platforms

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type store struct {
	mu    sync.RWMutex
	file  string
	state State
}

func openStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &store{file: filepath.Join(dir, "native-platforms.json"), state: State{Version: 1, Auto: true, Accounts: []Account{}, ProviderAuto: map[string]bool{}, Locks: map[string]bool{}, Disabled: map[string]map[string]bool{}}}
	raw, err := os.ReadFile(s.file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(raw, &s.state); err != nil {
			return nil, fmt.Errorf("invalid native platform store: %w", err)
		}
		if s.state.Version != 1 {
			return nil, errors.New("unsupported native platform store version")
		}
	}
	if s.state.ProviderAuto == nil {
		s.state.ProviderAuto = map[string]bool{}
	}
	if s.state.Locks == nil {
		s.state.Locks = map[string]bool{}
	}
	if s.state.Disabled == nil {
		s.state.Disabled = map[string]map[string]bool{}
	}
	return s, nil
}
func cloneState(state State) State {
	raw, _ := json.Marshal(state)
	var result State
	_ = json.Unmarshal(raw, &result)
	return result
}
func (s *store) snapshot() State { s.mu.RLock(); defer s.mu.RUnlock(); return cloneState(s.state) }
func (s *store) update(change func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	if err := change(&next); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(s.file), "native-platforms-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(raw)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, s.file); err != nil {
		return err
	}
	s.state = next
	return nil
}
func accountByID(state *State, id string) *Account {
	for i := range state.Accounts {
		if state.Accounts[i].ID == id {
			return &state.Accounts[i]
		}
	}
	return nil
}
