package config

import (
	"encoding/json"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Cursor spend guard defaults.
const (
	DefaultCursorSpendGuardMaxOverageUSD  = 20.0
	DefaultCursorSpendGuardPollInterval   = "60s"
	DefaultCursorSpendGuardGracePolls     = 3
	DefaultCursorSpendGuardRequestReserve = 0.50
	minCursorSpendGuardPollInterval       = 15 * time.Second
)

// CursorSpendGuardConfig caps Cursor on-demand spend once a credential's included quota is used up.
type CursorSpendGuardConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MaxOverageUSD is the most on-demand spend allowed per credential and billing period.
	MaxOverageUSD float64 `yaml:"max-overage-usd" json:"max-overage-usd"`
	// PollInterval is how often Cursor usage is read per credential (Go duration, at least 15s).
	PollInterval string `yaml:"poll-interval" json:"poll-interval"`
	// FailClosedWhenUnknown blocks Cursor when usage cannot be read (see UnknownGracePolls).
	FailClosedWhenUnknown bool `yaml:"fail-closed-when-unknown" json:"fail-closed-when-unknown"`
	// UnknownGracePolls is how many consecutive failed polls are tolerated before blocking while the
	// included quota is not known to be exhausted.
	UnknownGracePolls int `yaml:"unknown-grace-polls" json:"unknown-grace-polls"`
	// DefaultRequestReserveUSD is the cost reserved for a request whose model has no recent cost.
	DefaultRequestReserveUSD float64 `yaml:"default-request-reserve-usd" json:"default-request-reserve-usd"`
}

// DefaultCursorSpendGuard returns the guard settings used when the section is absent.
func DefaultCursorSpendGuard() CursorSpendGuardConfig {
	return CursorSpendGuardConfig{
		MaxOverageUSD:            DefaultCursorSpendGuardMaxOverageUSD,
		PollInterval:             DefaultCursorSpendGuardPollInterval,
		FailClosedWhenUnknown:    true,
		UnknownGracePolls:        DefaultCursorSpendGuardGracePolls,
		DefaultRequestReserveUSD: DefaultCursorSpendGuardRequestReserve,
	}
}

// IsZero reports the default (disabled) settings, which the YAML encoder omits like an absent section.
func (g CursorSpendGuardConfig) IsZero() bool {
	return g == DefaultCursorSpendGuard()
}

type cursorSpendGuardFields CursorSpendGuardConfig

// UnmarshalYAML decodes the section over the defaults, so omitted fields keep their defaults.
func (g *CursorSpendGuardConfig) UnmarshalYAML(node *yaml.Node) error {
	fields := cursorSpendGuardFields(DefaultCursorSpendGuard())
	if err := node.Decode(&fields); err != nil {
		return err
	}
	*g = CursorSpendGuardConfig(fields)
	return nil
}

// UnmarshalJSON decodes the section over the defaults.
func (g *CursorSpendGuardConfig) UnmarshalJSON(data []byte) error {
	fields := cursorSpendGuardFields(DefaultCursorSpendGuard())
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*g = CursorSpendGuardConfig(fields)
	return nil
}

// Sanitize clamps invalid values to safe ones.
func (g *CursorSpendGuardConfig) Sanitize() {
	if g.MaxOverageUSD < 0 {
		g.MaxOverageUSD = 0
	}
	g.PollInterval = strings.TrimSpace(g.PollInterval)
	if d, err := time.ParseDuration(g.PollInterval); err != nil || d <= 0 {
		g.PollInterval = DefaultCursorSpendGuardPollInterval
	} else if d < minCursorSpendGuardPollInterval {
		g.PollInterval = minCursorSpendGuardPollInterval.String()
	}
	if g.UnknownGracePolls < 1 {
		g.UnknownGracePolls = 1
	}
	if g.DefaultRequestReserveUSD < 0 {
		g.DefaultRequestReserveUSD = 0
	}
}

// PollEvery returns the poll interval as a duration, at least 15s.
func (g CursorSpendGuardConfig) PollEvery() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(g.PollInterval))
	if err != nil || d <= 0 {
		d, _ = time.ParseDuration(DefaultCursorSpendGuardPollInterval)
	}
	if d < minCursorSpendGuardPollInterval {
		d = minCursorSpendGuardPollInterval
	}
	return d
}
