package cost

import (
	"fmt"
	"sync"
	"time"
)

// AlertSeverity describes how urgent a budget alert is.
type AlertSeverity int

const (
	// SeverityInfo is issued when a soft threshold is reached.
	SeverityInfo AlertSeverity = iota
	// SeverityWarning is issued at an intermediate threshold.
	SeverityWarning
	// SeverityCritical is issued when a hard budget limit is reached or exceeded.
	SeverityCritical
)

// String returns the human-readable label for the severity.
func (s AlertSeverity) String() string {
	switch s {
	case SeverityInfo:
		return "INFO"
	case SeverityWarning:
		return "WARNING"
	case SeverityCritical:
		return "CRITICAL"
	default:
		return "UNKNOWN"
	}
}

// Alert is an immutable budget-threshold notification.
type Alert struct {
	// TenantID identifies which tenant triggered the alert.
	TenantID string

	// Severity is INFO, WARNING, or CRITICAL.
	Severity AlertSeverity

	// Message is a human-readable description.
	Message string

	// CurrentCost is the tenant's accumulated cost at alert time.
	CurrentCost float64

	// BudgetLimit is the threshold that was breached.
	BudgetLimit float64

	// Fraction is CurrentCost / BudgetLimit.
	Fraction float64

	// TriggeredAt is the wall-clock time the alert was generated.
	TriggeredAt time.Time
}

// AlertHandler is a callback invoked when an alert is generated.
// It must be non-blocking; heavy processing should be done in a goroutine.
type AlertHandler func(Alert)

// BudgetRule defines soft and hard spending limits for a tenant.
type BudgetRule struct {
	// TenantID is the tenant this rule applies to.
	// Use "*" to set a default rule applied to any tenant without a specific rule.
	TenantID string

	// SoftLimit triggers a SeverityWarning alert.
	SoftLimit float64

	// HardLimit triggers a SeverityCritical alert.
	// HardLimit must be >= SoftLimit; it may be zero to disable the hard limit.
	HardLimit float64

	// InfoFraction, if > 0, triggers a SeverityInfo alert when
	// currentCost / HardLimit >= InfoFraction.  Typical value: 0.5.
	InfoFraction float64
}

// AlertManager evaluates per-tenant BudgetRules after every cost event
// and emits Alerts to registered handlers.
// It is safe for concurrent use.
type AlertManager struct {
	mu       sync.RWMutex
	rules    map[string]BudgetRule
	handlers []AlertHandler
	alerts   []Alert
	// fired tracks which (tenantID, severity) pairs have already fired to
	// prevent duplicate alerts from flooding handlers.
	fired map[string]AlertSeverity
}

// NewAlertManager creates an AlertManager with no rules or handlers.
func NewAlertManager() *AlertManager {
	return &AlertManager{
		rules:  make(map[string]BudgetRule),
		fired:  make(map[string]AlertSeverity),
		alerts: []Alert{},
	}
}

// AddHandler registers h to be called whenever an alert is generated.
func (am *AlertManager) AddHandler(h AlertHandler) {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.handlers = append(am.handlers, h)
}

// SetRule installs or replaces the BudgetRule for the tenant identified by rule.TenantID.
func (am *AlertManager) SetRule(rule BudgetRule) {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.rules[rule.TenantID] = rule
}

// RemoveRule deletes the BudgetRule for tenantID, if present.
func (am *AlertManager) RemoveRule(tenantID string) {
	am.mu.Lock()
	defer am.mu.Unlock()
	delete(am.rules, tenantID)
}

// Check evaluates currentCost against the rules for tenantID and fires any
// outstanding alerts.  It should be called after every RecordOp or RecordStorage.
func (am *AlertManager) Check(tenantID string, currentCost float64) {
	am.mu.Lock()
	defer am.mu.Unlock()

	rule, ok := am.ruleFor(tenantID)
	if !ok {
		return
	}

	am.evaluate(tenantID, currentCost, rule)
}

// Alerts returns a copy of all alerts generated so far (across all tenants).
func (am *AlertManager) Alerts() []Alert {
	am.mu.RLock()
	defer am.mu.RUnlock()
	out := make([]Alert, len(am.alerts))
	copy(out, am.alerts)
	return out
}

// AlertsFor returns a copy of all alerts generated for tenantID.
func (am *AlertManager) AlertsFor(tenantID string) []Alert {
	am.mu.RLock()
	defer am.mu.RUnlock()
	var out []Alert
	for _, a := range am.alerts {
		if a.TenantID == tenantID {
			out = append(out, a)
		}
	}
	return out
}

// ClearAlerts discards all stored alerts and resets the fired-state so that
// rules can re-trigger on subsequent Check calls.
func (am *AlertManager) ClearAlerts() {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.alerts = am.alerts[:0]
	am.fired = make(map[string]AlertSeverity)
}

// ruleFor returns the rule for tenantID, falling back to the "*" wildcard.
// Caller must hold am.mu (any lock level).
func (am *AlertManager) ruleFor(tenantID string) (BudgetRule, bool) {
	if r, ok := am.rules[tenantID]; ok {
		return r, true
	}
	if r, ok := am.rules["*"]; ok {
		return r, true
	}
	return BudgetRule{}, false
}

// evaluate fires alerts for tenantID based on currentCost and rule.
// Caller must hold am.mu write lock.
func (am *AlertManager) evaluate(tenantID string, currentCost float64, rule BudgetRule) {
	// Hard limit check (CRITICAL).
	if rule.HardLimit > 0 && currentCost >= rule.HardLimit {
		am.maybeFireAlert(tenantID, SeverityCritical, currentCost, rule.HardLimit,
			fmt.Sprintf("tenant %s has reached hard budget limit: $%.4f / $%.4f",
				tenantID, currentCost, rule.HardLimit))
		return // no point firing lower-severity alerts if critical already fired
	}

	// Soft limit check (WARNING).
	if rule.SoftLimit > 0 && currentCost >= rule.SoftLimit {
		am.maybeFireAlert(tenantID, SeverityWarning, currentCost, rule.SoftLimit,
			fmt.Sprintf("tenant %s has exceeded soft budget limit: $%.4f / $%.4f",
				tenantID, currentCost, rule.SoftLimit))
	}

	// Info fraction check (INFO).
	if rule.InfoFraction > 0 && rule.HardLimit > 0 {
		fraction := currentCost / rule.HardLimit
		if fraction >= rule.InfoFraction {
			am.maybeFireAlert(tenantID, SeverityInfo, currentCost, rule.HardLimit,
				fmt.Sprintf("tenant %s has used %.0f%% of hard budget: $%.4f / $%.4f",
					tenantID, fraction*100, currentCost, rule.HardLimit))
		}
	}
}

// maybeFireAlert fires alert only if the same (tenant, severity) hasn't already fired.
// Caller must hold am.mu write lock.
func (am *AlertManager) maybeFireAlert(tenantID string, severity AlertSeverity, currentCost, limit float64, msg string) {
	key := tenantID + ":" + severity.String()
	if prev, already := am.fired[key]; already && prev >= severity {
		return
	}
	am.fired[key] = severity

	fraction := 0.0
	if limit > 0 {
		fraction = currentCost / limit
	}

	alert := Alert{
		TenantID:    tenantID,
		Severity:    severity,
		Message:     msg,
		CurrentCost: currentCost,
		BudgetLimit: limit,
		Fraction:    fraction,
		TriggeredAt: time.Now(),
	}
	am.alerts = append(am.alerts, alert)

	// Invoke handlers without holding the lock to avoid deadlocks.
	handlers := make([]AlertHandler, len(am.handlers))
	copy(handlers, am.handlers)
	go func() {
		for _, h := range handlers {
			h(alert)
		}
	}()
}
