// Package platforms implements native Go account adapters; no subprocess or JS runtime is used.
package platforms

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type Config struct {
	Enabled bool   `json:"enabled"`
	DataDir string `json:"data_dir"`
}
type Credential map[string]any
type Provider struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Unit  string `json:"unit"`
	Auto  bool   `json:"auto"`
	Claim bool   `json:"claim"`
	Login string `json:"loginMode"`
}
type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int64  `json:"context_window,omitempty"`
	MaxTokens     int64  `json:"max_tokens,omitempty"`
	Vision        bool   `json:"vision,omitempty"`
}
type Package struct {
	Name      string  `json:"name"`
	Unit      string  `json:"unit"`
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
	Active    bool    `json:"active"`
	ExpiresAt int64   `json:"deductionEndTime,omitempty"`
}
type Balance struct {
	Total        float64   `json:"total"`
	Packages     []Package `json:"packages"`
	ExpiredTotal float64   `json:"expiredTotal"`
}
type Outcome struct {
	Kind        string  `json:"kind"`
	Credit      float64 `json:"credit,omitempty"`
	Unit        string  `json:"unit,omitempty"`
	Message     string  `json:"message,omitempty"`
	CoversToday bool    `json:"coversToday"`
}
type Checkin struct {
	LastDate    string   `json:"lastDate,omitempty"`
	LastAttempt int64    `json:"lastAttempt,omitempty"`
	Error       string   `json:"error,omitempty"`
	Outcome     *Outcome `json:"outcome,omitempty"`
}
type Account struct {
	ID         string     `json:"id"`
	Provider   string     `json:"provider"`
	Nickname   string     `json:"nickname"`
	Enabled    bool       `json:"enabled"`
	CreatedAt  int64      `json:"createdAt"`
	Credential Credential `json:"credential,omitempty"`
	Checkin    Checkin    `json:"checkin"`
	Balance    *Balance   `json:"balance,omitempty"`
	BalanceAt  int64      `json:"balanceAt,omitempty"`
}

func (a Account) public() Account { a.Credential = nil; return a }

type State struct {
	Version      int                        `json:"version"`
	Accounts     []Account                  `json:"accounts"`
	Auto         bool                       `json:"auto"`
	ProviderAuto map[string]bool            `json:"provider_auto"`
	Locks        map[string]bool            `json:"permanent_locks"`
	Disabled     map[string]map[string]bool `json:"disabled_models"`
}

type Adapter interface {
	Info() Provider
	Models(context.Context, Credential) ([]Model, error)
	Balance(context.Context, Credential) (*Balance, error)
	Claim(context.Context, Credential) (Outcome, error)
	Refresh(context.Context, Credential) (Credential, error)
	Chat(context.Context, Credential, map[string]any, func(json.RawMessage) error) error
}
type SMSAdapter interface {
	SendSMS(context.Context, string) (string, error)
	LoginSMS(context.Context, string, string, string) (Credential, error)
}
type DeviceAuthorization struct {
	DeviceCode string
	Verifier   string
	URL        string
	Interval   time.Duration
	Expires    time.Duration
}
type DeviceAdapter interface {
	BeginLogin(context.Context) (DeviceAuthorization, error)
	PollLogin(context.Context, DeviceAuthorization) (Credential, bool, error)
}
type LoginResult struct {
	Credential Credential
	Err        error
}
type BrowserAuthorization struct {
	URL    string
	Result <-chan LoginResult
}
type BrowserAdapter interface {
	BeginBrowserLogin(context.Context) (BrowserAuthorization, error)
}
type RemoteError struct {
	Message string
	Status  int
}

func (e *RemoteError) Error() string { return e.Message }

type network struct {
	http *http.Client
	chat *http.Client
	api  string
	auth string
}

func defaultNetwork(api, auth string) network {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	return network{http: &http.Client{Timeout: 60 * time.Second}, chat: &http.Client{Transport: idleTransport{base: transport, timeout: 300 * time.Second}}, api: api, auth: auth}
}
