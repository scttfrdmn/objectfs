package cost

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAlertManager_NoRule_NoAlert(t *testing.T) {
	t.Parallel()
	am := NewAlertManager()
	am.Check("alice", 100.0)
	assert.Empty(t, am.Alerts())
}

func TestAlertManager_SoftLimit_Warning(t *testing.T) {
	t.Parallel()
	am := NewAlertManager()
	am.SetRule(BudgetRule{TenantID: "alice", SoftLimit: 10.0, HardLimit: 50.0})
	am.Check("alice", 15.0)
	alerts := am.AlertsFor("alice")
	assert.Len(t, alerts, 1)
	assert.Equal(t, SeverityWarning, alerts[0].Severity)
	assert.Equal(t, "alice", alerts[0].TenantID)
}

func TestAlertManager_HardLimit_Critical(t *testing.T) {
	t.Parallel()
	am := NewAlertManager()
	am.SetRule(BudgetRule{TenantID: "alice", SoftLimit: 10.0, HardLimit: 50.0})
	am.Check("alice", 60.0)
	alerts := am.AlertsFor("alice")
	assert.Len(t, alerts, 1)
	assert.Equal(t, SeverityCritical, alerts[0].Severity)
}

func TestAlertManager_InfoFraction_Info(t *testing.T) {
	t.Parallel()
	am := NewAlertManager()
	am.SetRule(BudgetRule{TenantID: "alice", SoftLimit: 10.0, HardLimit: 100.0, InfoFraction: 0.5})
	am.Check("alice", 55.0) // 55% of hard limit → info
	var hasInfo bool
	for _, a := range am.AlertsFor("alice") {
		if a.Severity == SeverityInfo {
			hasInfo = true
		}
	}
	assert.True(t, hasInfo)
}

func TestAlertManager_NoDoubleFireSameSeverity(t *testing.T) {
	t.Parallel()
	am := NewAlertManager()
	am.SetRule(BudgetRule{TenantID: "alice", SoftLimit: 10.0, HardLimit: 50.0})
	am.Check("alice", 15.0)
	am.Check("alice", 20.0)
	// Second call should not re-fire the warning.
	alerts := am.AlertsFor("alice")
	assert.Len(t, alerts, 1)
}

func TestAlertManager_ClearAlerts_Resets(t *testing.T) {
	t.Parallel()
	am := NewAlertManager()
	am.SetRule(BudgetRule{TenantID: "alice", SoftLimit: 10.0, HardLimit: 50.0})
	am.Check("alice", 15.0)
	am.ClearAlerts()
	assert.Empty(t, am.Alerts())
	// Should be able to fire again after clear.
	am.Check("alice", 20.0)
	assert.Len(t, am.Alerts(), 1)
}

func TestAlertManager_Handler_Called(t *testing.T) {
	t.Parallel()
	am := NewAlertManager()
	am.SetRule(BudgetRule{TenantID: "alice", SoftLimit: 10.0, HardLimit: 50.0})

	var mu sync.Mutex
	var received []Alert
	am.AddHandler(func(a Alert) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, a)
	})

	am.Check("alice", 15.0)
	// Give the handler goroutine time to run.
	time.Sleep(10 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, received, 1)
	assert.Equal(t, SeverityWarning, received[0].Severity)
}

func TestAlertManager_WildcardRule(t *testing.T) {
	t.Parallel()
	am := NewAlertManager()
	am.SetRule(BudgetRule{TenantID: "*", SoftLimit: 5.0, HardLimit: 20.0})
	am.Check("unknown-tenant", 10.0)
	alerts := am.AlertsFor("unknown-tenant")
	assert.Len(t, alerts, 1)
	assert.Equal(t, SeverityWarning, alerts[0].Severity)
}

func TestAlertManager_RemoveRule(t *testing.T) {
	t.Parallel()
	am := NewAlertManager()
	am.SetRule(BudgetRule{TenantID: "alice", SoftLimit: 10.0, HardLimit: 50.0})
	am.RemoveRule("alice")
	am.Check("alice", 100.0)
	assert.Empty(t, am.Alerts())
}

func TestAlertManager_AlertFraction_Correct(t *testing.T) {
	t.Parallel()
	am := NewAlertManager()
	am.SetRule(BudgetRule{TenantID: "alice", SoftLimit: 10.0, HardLimit: 50.0})
	am.Check("alice", 15.0)
	alerts := am.AlertsFor("alice")
	assert.Len(t, alerts, 1)
	// fraction = 15 / 10 = 1.5 (cost/softLimit)
	assert.InDelta(t, 1.5, alerts[0].Fraction, 0.001)
}

func TestAlertSeverity_String(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "INFO", SeverityInfo.String())
	assert.Equal(t, "WARNING", SeverityWarning.String())
	assert.Equal(t, "CRITICAL", SeverityCritical.String())
}

func TestAlertManager_BelowAllThresholds_NoAlert(t *testing.T) {
	t.Parallel()
	am := NewAlertManager()
	am.SetRule(BudgetRule{TenantID: "alice", SoftLimit: 100.0, HardLimit: 500.0, InfoFraction: 0.5})
	am.Check("alice", 1.0)
	assert.Empty(t, am.Alerts())
}
