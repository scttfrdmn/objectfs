package metrics

import (
	"context"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/scttfrdmn/objectfs/internal/testhttp"
)

// #204's exporter half. The S3 backend tracked whether Transfer Acceleration was in effect and nothing
// outside its own package could read it, so a mount that had silently dropped to the standard endpoint
// reported acceleration enabled and the throughput loss appeared in no scrape, log, or health check.
//
// The tests here are about this family's shape rather than about the fallback: that the booleans survive
// the trip as 0 and 1, that a gauge is what these are, and that the family is present even when
// acceleration is off — which is the deliberate difference from objectfs_predictive_cache, whose absence
// is meaningful.

// TestAccelerationGaugeCarriesOneSeriesPerStatistic pins the shape.
//
// One family labeled by statistic, matching objectfs_predictive_cache: the set of statistics will grow,
// and a new label value is something dashboards and SDKs pick up for free while a new metric name is a
// change every consumer has to follow.
func TestAccelerationGaugeCarriesOneSeriesPerStatistic(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	want := map[string]float64{
		"configured":           1,
		"active":               0,
		"requests":             412,
		"bytes":                1 << 24,
		"fallbacks":            1,
		"avg_latency_seconds":  0.0184,
		"retry_period_seconds": 300,
	}

	c.UpdateS3Acceleration(want)

	got := accelerationSeries(t, c)

	for name, value := range want {
		if got[name] != value {
			t.Errorf(`objectfs_s3_acceleration{statistic=%q} = %v, want %v`, name, got[name], value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the family carries %d series, want %d: %v", len(got), len(want), got)
	}
}

// TestAccelerationGaugeTracksTheLatestSnapshot is what makes the fallback and the recovery visible.
//
// These values are a snapshot of another package's state, read on a tick. A gauge that accumulated
// would make `active` climb past 1 and stop being a boolean; one that ignored later publications would
// freeze at the mount's opening state — which is the exact failure #204 is about, since the opening
// state is "acceleration enabled" and the interesting state is the one it changes to.
func TestAccelerationGaugeTracksTheLatestSnapshot(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	// A mount that starts accelerating, falls back, and recovers.
	c.UpdateS3Acceleration(map[string]float64{"active": 1, "fallbacks": 0})
	c.UpdateS3Acceleration(map[string]float64{"active": 0, "fallbacks": 1})
	c.UpdateS3Acceleration(map[string]float64{"active": 1, "fallbacks": 1})

	got := accelerationSeries(t, c)

	if got["active"] != 1 {
		t.Errorf("active = %v after 1 → 0 → 1; the gauge does not track the latest snapshot, so a "+
			"recovery would never appear in a scrape", got["active"])
	}
	if got["fallbacks"] != 1 {
		t.Errorf("fallbacks = %v after publishing 0, 1, 1; a snapshot of a running total is being "+
			"accumulated rather than set", got["fallbacks"])
	}
}

// TestAccelerationGaugeCarriesTheOperatorLabels asserts the new family is not an exception to
// monitoring.metrics.custom_labels, which is documented as attached to every exported metric.
//
// TestCustomLabelsReachEveryMetric asserts over every family, but only over families that have been
// observed — so a family added without ConstLabels and without an observation there passes it.
func TestAccelerationGaugeCarriesTheOperatorLabels(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(&Config{
		Enabled: true,
		Addr:    anyLoopbackAddr,
		Labels:  map[string]string{"service": "objectfs", "cluster": "research-west"},
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.UpdateS3Acceleration(map[string]float64{"configured": 1})

	for _, mf := range gather(t, c) {
		if mf.GetName() != "objectfs_s3_acceleration" {
			continue
		}

		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}

			for name, value := range map[string]string{"service": "objectfs", "cluster": "research-west"} {
				if labels[name] != value {
					t.Errorf("objectfs_s3_acceleration is missing the operator label %s=%s; it carried %v",
						name, value, labels)
				}
			}
		}
	}
}

// TestUpdateS3AccelerationHonorsDisabled asserts a disabled collector builds no series.
//
// A disabled collector has no registry, so publishing into it must not dereference one — and the adapter
// registers this publisher unconditionally, before consulting whether metrics are on.
func TestUpdateS3AccelerationHonorsDisabled(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(&Config{Enabled: false})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	// The assertion is that this does not panic on a nil GaugeVec and nil registry.
	c.UpdateS3Acceleration(map[string]float64{"configured": 0})
}

// TestAccelerationStatisticsReachAScrape closes the loop over HTTP.
//
// Every other test here reads the registry, which cannot show whether a series survives the exposition
// format. The label values are what an SDK and a dashboard key on, so they are checked as they appear on
// the wire.
func TestAccelerationStatisticsReachAScrape(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.UpdateS3Acceleration(map[string]float64{
		"configured": 1,
		"active":     0,
		"fallbacks":  3,
	})

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	body := testhttp.Get(t, c.Addr(), c.config.Path, "Start bound no listener")

	for _, want := range []string{
		"objectfs_s3_acceleration",
		`statistic="configured"`,
		`statistic="active"`,
		`statistic="fallbacks"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("a scrape of /metrics does not contain %q; body:\n%s", want, body)
		}
	}
}

// accelerationSeries returns the acceleration family as statistic name → value.
func accelerationSeries(t *testing.T, c *Collector) map[string]float64 {
	t.Helper()

	out := map[string]float64{}

	for _, mf := range gather(t, c) {
		if mf.GetName() != "objectfs_s3_acceleration" {
			continue
		}

		if kind := mf.GetType(); kind != dto.MetricType_GAUGE {
			t.Errorf("objectfs_s3_acceleration is a %v, want a gauge: the values are a snapshot of the "+
				"backend's state, including two booleans, and a counter has no Set", kind)
		}

		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "statistic" {
					out[l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}

	if len(out) == 0 {
		t.Fatalf("no objectfs_s3_acceleration series in the registry")
	}

	return out
}
