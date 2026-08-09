/**
 * Tests for the Prometheus scrape parsing in ./prometheus.
 *
 * Every test reads `sdks/testdata/metrics-scrape.txt`, which is not a hand-written sample: it is
 * a real /metrics response captured from internal/metrics.Collector, and
 * TestSDKFixtureMatchesTheLiveScrape in internal/metrics regenerates and compares it on every Go
 * test run. So a metric renamed in Go fails the Go suite, and the corrected fixture then fails
 * these tests -- which is the whole point.
 *
 * These assertions mirror sdks/python/tests/test_monitoring.py case for case, deliberately: two
 * independent parsers agreeing on one captured scrape is a stronger statement than either alone,
 * and a divergence between them is a real defect in one of the two SDKs.
 *
 * Run with the repo's configured runner:
 *
 *     cd sdks/javascript && npm install && npm test
 *
 * The assertions are node:assert rather than jest's `expect`, because ./prometheus deliberately
 * imports no transport and these tests deliberately depend on nothing but the standard library and
 * the module under test. jest supplies the harness; it is not load-bearing for what is asserted.
 */

import { describe, test } from '@jest/globals';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import {
  parseScrape,
  parseSampleLine,
  processMetrics,
  extractCacheStats,
  extractIOStats,
  extractOperationStats,
  extractErrorStats,
  extractConnectionStats,
} from './prometheus';
import { RawMetrics } from './types';

const FIXTURE = join(__dirname, '..', '..', 'testdata', 'metrics-scrape.txt');
const scrapeText = readFileSync(FIXTURE, 'utf8');
const parsed = parseScrape(scrapeText);

describe('parseSampleLine', () => {
  test('separates a metric name from its label block', () => {
    const sample = parseSampleLine(
      'objectfs_cache_requests_total{service="objectfs",type="hit"} 3'
    );

    // The old first-space split produced
    // 'objectfs_cache_requests_total{service="objectfs",type="hit"}' as the name -- a string no
    // lookup anywhere in the SDK could ever match.
    assert.equal(sample?.name, 'objectfs_cache_requests_total');
    assert.deepEqual(sample?.labels, { service: 'objectfs', type: 'hit' });
    assert.equal(sample?.value, 3);
  });

  test('parses an unlabelled line', () => {
    const sample = parseSampleLine('some_metric 42');

    assert.equal(sample?.name, 'some_metric');
    assert.deepEqual(sample?.labels, {});
    assert.equal(sample?.value, 42);
  });

  test('parses scientific notation', () => {
    // Prometheus writes large float values this way: cache_size_bytes is a float gauge, so
    // 1 MiB is exported as 1.048576e+06, not 1048576.
    const sample = parseSampleLine(
      'objectfs_cache_size_bytes{level="L1"} 1.048576e+06'
    );

    assert.equal(sample?.value, 1048576);
  });

  test('keeps the +Inf histogram bucket', () => {
    // A normal sample line. Rejecting it would drop the histogram's total count.
    const sample = parseSampleLine(
      'objectfs_operation_duration_seconds_bucket{operation="read",le="+Inf"} 2'
    );

    assert.equal(sample?.labels['le'], '+Inf');
    assert.equal(sample?.value, 2);
  });

  test('a comma inside a label value is data, not a separator', () => {
    // Splitting the label block on ',' would produce two broken fragments and lose the sample.
    const sample = parseSampleLine(
      'objectfs_errors_total{operation="read",type="a, b"} 1'
    );

    assert.deepEqual(sample?.labels, { operation: 'read', type: 'a, b' });
    assert.equal(sample?.value, 1);
  });

  test('an escaped quote inside a label value', () => {
    const sample = parseSampleLine(
      'objectfs_errors_total{type="say \\"hi\\""} 1'
    );

    assert.deepEqual(sample?.labels, { type: 'say "hi"' });
  });

  test('a brace inside a label value does not truncate the block', () => {
    // lastIndexOf('}') rather than indexOf.
    const sample = parseSampleLine(
      'objectfs_errors_total{type="a}b",operation="read"} 7'
    );

    assert.equal(sample?.name, 'objectfs_errors_total');
    assert.deepEqual(sample?.labels, { type: 'a}b', operation: 'read' });
    assert.equal(sample?.value, 7);
  });

  test('a trailing timestamp is not the value', () => {
    // The format permits one. Reading it as the value would report a millisecond epoch as a
    // metric.
    assert.equal(parseSampleLine('some_metric 5 1698000000000')?.value, 5);
  });

  test('rejects lines that are not samples', () => {
    for (const line of [
      '# HELP objectfs_operations_total Total number of operations',
      '# TYPE objectfs_operations_total counter',
      'no_value_at_all',
      'bad_value not_a_number',
      'objectfs_thing{unterminated="x" 5',
    ]) {
      assert.equal(
        parseSampleLine(line),
        null,
        `${line} should not parse as a sample`
      );
    }
  });
});

describe('parseScrape', () => {
  test('skips comments and finds samples', () => {
    assert.ok(
      parsed.samples.length > 0,
      'the fixture parsed to no samples at all'
    );
    for (const sample of parsed.samples) {
      assert.ok(!sample.name.startsWith('#'));
      assert.ok(!sample.name.includes('{'));
    }
  });

  test('keeps every sample of a repeated name', () => {
    // hit and miss are two samples of one name. A map keyed by name would keep one.
    const requests = parsed.samples.filter(
      (s) => s.name === 'objectfs_cache_requests_total'
    );

    assert.equal(requests.length, 2);
  });

  test('every family the collector exports is present', () => {
    // Enumerated from internal/metrics.initMetrics -- the SDK's half of the contract that
    // TestExportedNamesAreTheOnesDocumentedAndScraped asserts from the Go side.
    const found = new Set(parsed.samples.map((s) => s.name));

    for (const name of [
      'objectfs_operations_total',
      'objectfs_operation_duration_seconds_sum',
      'objectfs_operation_size_bytes_sum',
      'objectfs_cache_requests_total',
      'objectfs_cache_size_bytes',
      'objectfs_active_connections',
      'objectfs_errors_total',
      'objectfs_predictive_cache',
    ]) {
      assert.ok(found.has(name), `${name} missing from the parsed scrape`);
    }
  });

  test('predictive statistics are labelled, not named', () => {
    // The predictive cache is one family labelled by `statistic` rather than a metric per number,
    // so the set of statistics can grow without a metric rename -- a change every SDK and dashboard
    // would otherwise have to follow. That only works if the parser keeps the label, so this
    // asserts on the label rather than on the family's presence.
    const statistics = new Map(
      parsed.samples
        .filter((s) => s.name === 'objectfs_predictive_cache')
        .map((s) => [s.labels['statistic'], s.value])
    );

    for (const name of [
      'predictions_total',
      'predictions_correct',
      'prediction_accuracy',
      'avg_confidence',
      'prefetch_requests',
      'prefetch_hits',
      'prefetch_bytes',
      'prefetch_waste',
      'prefetch_efficiency',
      'evictions_total',
      'evictions_intelligent',
    ]) {
      assert.ok(statistics.has(name), `statistic=${name} missing from the parsed scrape`);
    }

    // A ratio, derived in Go when the totals are written rather than at scrape time. 61 of 186 is
    // deliberately not a round number: 0, 0.5 and 1 are all values a broken parser can produce by
    // accident.
    assert.ok(Math.abs((statistics.get('prediction_accuracy') ?? 0) - 61 / 186) < 1e-6);
    assert.equal(statistics.get('predictions_total'), 186);
  });

  test('operator labels survive the parse', () => {
    // monitoring.metrics.custom_labels attaches these to every series. An SDK that dropped them
    // could not tell two nodes' metrics apart.
    for (const sample of parsed.samples) {
      assert.equal(sample.labels['service'], 'objectfs');
    }
  });
});

describe('extractCacheStats', () => {
  const stats = extractCacheStats(parsed);

  test('hits and misses come from the type label', () => {
    assert.equal(stats.hits, 3);
    assert.equal(stats.misses, 1);
  });

  test('hit rate is derived', () => {
    // 3 hits, 1 miss. Deliberately not 0, 0.5 or 1: a broken parser can produce those by
    // accident, and 0.75 requires having read both labelled samples correctly.
    assert.equal(stats.hitRate, 0.75);
  });

  test('per-level sizes', () => {
    assert.deepEqual(stats.levels, { L1: 1048576, L2: 5242880 });
    assert.equal(stats.size, 1048576 + 5242880);
  });

  test('no cache requests yields no hit rate', () => {
    // An idle mount has served no cache request, and that is a different fact from a hit rate of
    // zero. Reporting 0 would read as a cache that never hits.
    const empty = extractCacheStats({ samples: [] });

    assert.equal(empty.hitRate, undefined);
    assert.equal(empty.hits, undefined);
  });

  test('misses only still gives a hit rate of zero', () => {
    // This was the live state of v0.10.0: RecordCacheHit had no caller anywhere in the repo, so
    // only misses were recorded. A hit rate of 0 is the correct reading of that scrape, and
    // exactly what an operator should have seen -- the old `if (hits && misses)` guard reported
    // nothing at all.
    const stats = extractCacheStats({
      samples: [
        {
          name: 'objectfs_cache_requests_total',
          labels: { type: 'miss' },
          value: 9,
        },
      ],
    });

    assert.equal(stats.hitRate, 0);
    assert.equal(stats.misses, 9);
  });
});

describe('extractOperationStats', () => {
  const stats = extractOperationStats(parsed);

  test('totals sum across labels', () => {
    // The fixture records read/success, read/error and write/success. A reader that took one
    // sample's value would report 1 for every field.
    assert.equal(stats?.total, 3);
    assert.equal(stats?.successful, 2);
    assert.equal(stats?.failed, 1);
  });

  test('per-operation breakdown', () => {
    assert.equal(stats?.byOperation['read']?.count, 2);
    assert.equal(stats?.byOperation['write']?.count, 1);
  });

  test('average duration from the histogram', () => {
    // Two reads at 12 ms and 9 ms: sum 0.021 s over 2 observations.
    const read = stats?.byOperation['read'];

    assert.ok(Math.abs((read?.durationSeconds ?? 0) - 0.021) < 1e-9);
    assert.ok(Math.abs((read?.avgDurationSeconds ?? 0) - 0.0105) < 1e-9);
  });

  test('byte totals', () => {
    assert.equal(stats?.byOperation['read']?.bytes, 8192);
    assert.equal(stats?.byOperation['write']?.bytes, 8192);
  });

  test('an empty scrape yields null', () => {
    assert.equal(extractOperationStats({ samples: [] }), null);
  });
});

describe('extractIOStats', () => {
  test('projects read and write operations', () => {
    const stats = extractIOStats(parsed);

    assert.equal(stats.readOperations, 2);
    assert.equal(stats.writeBytes, 8192);
  });

  test('operations not recorded are absent, not zero', () => {
    // As of v0.10.1 the FUSE layer does not call RecordOperation for reads and writes, so a real
    // mount's scrape has no read/write samples and this is {}. Returning zeros would report an
    // idle filesystem instead of an unrecorded one.
    const stats = extractIOStats({
      samples: [
        {
          name: 'objectfs_operations_total',
          labels: { operation: 'prefetch', status: 'success' },
          value: 4,
        },
      ],
    });

    assert.deepEqual(stats, {});
  });
});

describe('extractErrorStats', () => {
  const stats = extractErrorStats(parsed);

  test('split by classification', () => {
    // The fixture's one RecordError call produces a 'timeout' classification, and the failed
    // RecordOperation produces a 'failure'. Both are errors_total samples.
    assert.equal(stats?.total, 2);
    assert.equal(stats?.byType['timeout'], 1);
    assert.equal(stats?.byType['failure'], 1);
  });

  test('summed per operation', () => {
    assert.equal(stats?.byOperation['read'], 2);
  });

  test('no errors yields null', () => {
    assert.equal(extractErrorStats({ samples: [] }), null);
  });
});

describe('extractConnectionStats', () => {
  test('reads the unlabelled gauge', () => {
    assert.deepEqual(extractConnectionStats(parsed), { active: 4 });
  });

  test('null when the gauge is absent', () => {
    assert.equal(extractConnectionStats({ samples: [] }), null);
  });
});

describe('processMetrics', () => {
  const processed = processMetrics(parsed, 1700000000000);

  test('the README example works', () => {
    // sdks/javascript/README.md prints metrics.cache.hitRate. That was undefined against a
    // healthy mount for the whole life of the SDK.
    assert.equal(processed.cache?.hitRate, 0.75);
  });

  test('sections present', () => {
    assert.ok(processed.cache);
    assert.ok(processed.io);
    assert.ok(processed.operations);
    assert.ok(processed.errors);
    assert.ok(processed.connections);
  });

  test('raw samples are kept', () => {
    // So a caller can read a metric the extractors do not surface, without re-scraping.
    assert.ok(processed.raw.samples.length > 0);
  });

  test('no invented sections', () => {
    // network, storage and distributed were built from metric names ObjectFS has never
    // exported, so each was permanently empty while advertising a whole subsystem's telemetry.
    const keys = Object.keys(processed);

    for (const absent of ['network', 'storage', 'distributed']) {
      assert.ok(!keys.includes(absent), `${absent} should not be a section`);
    }
  });

  test('the timestamp is the one supplied', () => {
    assert.equal(processed.timestamp, 1700000000000);
  });
});

describe('agreement with the Python SDK', () => {
  test('both SDKs read the same numbers off the same scrape', () => {
    // These are the exact values asserted in sdks/python/tests/test_monitoring.py. Two
    // independently written parsers agreeing on one captured scrape is the strongest check
    // available here; a divergence is a real defect in one of them.
    const cache = extractCacheStats(parsed);
    const ops = extractOperationStats(parsed);
    const errors = extractErrorStats(parsed);

    assert.deepEqual(
      {
        hits: cache.hits,
        misses: cache.misses,
        hitRate: cache.hitRate,
        size: cache.size,
        opsTotal: ops?.total,
        opsSuccessful: ops?.successful,
        opsFailed: ops?.failed,
        errorTotal: errors?.total,
        connections: extractConnectionStats(parsed)?.active,
      },
      {
        hits: 3,
        misses: 1,
        hitRate: 0.75,
        size: 6291456,
        opsTotal: 3,
        opsSuccessful: 2,
        opsFailed: 1,
        errorTotal: 2,
        connections: 4,
      }
    );
  });
});

// Silences "declared but never used" under noUnusedLocals if it is ever enabled; also documents
// that the fixture is parsed as RawMetrics and nothing looser.
const _shape: RawMetrics = parsed;
void _shape;
