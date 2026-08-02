/**
 * Prometheus text-exposition parsing for ObjectFS metrics.
 *
 * These are free functions in a module of their own, with no dependency on axios or on any
 * transport, because that is what makes them testable: `prometheus.test.ts` parses
 * `sdks/testdata/metrics-scrape.txt` -- a real /metrics response captured from
 * internal/metrics.Collector, which internal/metrics.TestSDKFixtureMatchesTheLiveScrape
 * regenerates and compares on every Go test run. So a metric renamed in Go fails the Go suite,
 * and the corrected fixture then fails these functions' tests, in the same commit.
 *
 * The absence of that link is why this code was broken. It lived inside MetricsCollector next
 * to an HTTP client, so exercising it meant standing up a server; nothing did, and nothing
 * compared what the SDK expected against what the exporter produced. The parser split each line
 * on its first space -- which fuses a metric's name to its label block -- and the extractors
 * then looked up names like `cache_hits` and `objectfs_io_read_operations_total` that ObjectFS
 * has never exported. Both halves were wrong, and each hid the other.
 */

import {
  RawMetrics,
  PrometheusSample,
  Metrics,
  CacheMetrics,
  IOMetrics,
  OperationMetrics,
  OperationDetail,
  ErrorMetrics,
  ConnectionMetrics,
} from './types';

/**
 * Parse a Prometheus text exposition into labelled samples.
 *
 * Samples are a list rather than a name-keyed record because a metric name identifies a family,
 * not a series: hit and miss are two samples of `objectfs_cache_requests_total`, and a map keyed
 * by name silently keeps whichever the exposition happened to list last.
 */
export function parseScrape(text: string): RawMetrics {
  const samples: PrometheusSample[] = [];

  for (const line of text.split('\n')) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith('#')) {
      continue;
    }

    const sample = parseSampleLine(trimmed);
    if (sample) {
      samples.push(sample);
    }
  }

  return { samples };
}

/**
 * Split one exposition line into a sample, or return null if it is not one.
 *
 * `NaN`, `+Inf` and `-Inf` are legal values, and the `+Inf` histogram bucket is an ordinary
 * line -- rejecting it would drop the histogram's total count. A trailing timestamp is permitted
 * by the format and is discarded: it is the scrape time, not the value.
 */
export function parseSampleLine(line: string): PrometheusSample | null {
  const brace = line.indexOf('{');

  if (brace === -1) {
    const parts = line.split(/\s+/);
    const raw = parts[1];
    if (raw === undefined) return null;

    const value = Number(raw);
    if (Number.isNaN(value) && raw !== 'NaN') return null;

    return { name: parts[0]!, labels: {}, value };
  }

  // lastIndexOf, so a '}' inside a label value cannot truncate the label block.
  const close = line.lastIndexOf('}');
  if (close < brace) return null;

  const labels = parseLabels(line.substring(brace + 1, close));
  if (labels === null) return null;

  const raw = line.substring(close + 1).trim().split(/\s+/)[0];
  if (!raw) return null;

  const value = Number(raw);
  if (Number.isNaN(value) && raw !== 'NaN') return null;

  return { name: line.substring(0, brace), labels, value };
}

/**
 * Parse the inside of a label block, or return null if it is malformed.
 *
 * Scanned character by character rather than split on ',' because a label value is an escaped
 * string and may itself contain a comma, a quote or a brace -- an error message in an
 * `objectfs_errors_total{type=...}` label, for instance. Splitting would break such a line into
 * fragments and lose the sample.
 */
export function parseLabels(text: string): Record<string, string> | null {
  const labels: Record<string, string> = {};
  const n = text.length;
  let i = 0;

  while (i < n) {
    const eq = text.indexOf('=', i);
    if (eq === -1) return null;

    const key = text.substring(i, eq).trim();
    let j = eq + 1;
    if (text[j] !== '"') return null;

    j += 1;
    let value = '';
    let terminated = false;
    while (j < n) {
      const ch = text[j]!;
      if (ch === '\\' && j + 1 < n) {
        const next = text[j + 1]!;
        value += next === 'n' ? '\n' : next;
        j += 2;
        continue;
      }
      if (ch === '"') {
        terminated = true;
        break;
      }
      value += ch;
      j += 1;
    }
    if (!terminated) return null;

    labels[key] = value;

    // Past the closing quote, then past the separator.
    i = j + 1;
    while (i < n && (text[i] === ',' || text[i] === ' ')) {
      i += 1;
    }
  }

  return labels;
}

/** Every sample of one metric family, in scrape order. */
function samplesFor(data: RawMetrics, name: string): PrometheusSample[] {
  return data.samples.filter((s) => s.name === name);
}

/**
 * Organize a parsed scrape into the shape `getMetrics()` returns.
 *
 * `timestamp` is passed in rather than read from the clock so that a test can assert on the
 * whole returned object.
 */
export function processMetrics(data: RawMetrics, timestamp: number): Metrics {
  const metrics: Metrics = {
    timestamp,
    raw: data,
    cache: extractCacheStats(data),
    io: extractIOStats(data),
  };

  // Assigned conditionally rather than always: exactOptionalPropertyTypes rejects an explicit
  // undefined, and a present-but-empty section cannot be told apart from one the mount is not
  // recording.
  const operations = extractOperationStats(data);
  if (operations) metrics.operations = operations;

  const errors = extractErrorStats(data);
  if (errors) metrics.errors = errors;

  const connections = extractConnectionStats(data);
  if (connections) metrics.connections = connections;

  return metrics;
}

/**
 * Cache statistics from `objectfs_cache_requests_total{type}` and
 * `objectfs_cache_size_bytes{level}`.
 *
 * This used to look for `cache_hits` and `objectfs_cache_hits_total`, neither of which any
 * version of ObjectFS has exported -- so `hitRate`, the one number the README example prints,
 * was never present.
 */
export function extractCacheStats(data: RawMetrics): CacheMetrics {
  const cache: CacheMetrics = {};

  for (const sample of samplesFor(data, 'objectfs_cache_requests_total')) {
    if (sample.labels['type'] === 'hit') cache.hits = sample.value;
    else if (sample.labels['type'] === 'miss') cache.misses = sample.value;
  }

  const levels: Record<string, number> = {};
  let total = 0;
  let anyLevel = false;
  for (const sample of samplesFor(data, 'objectfs_cache_size_bytes')) {
    levels[sample.labels['level'] ?? ''] = sample.value;
    total += sample.value;
    anyLevel = true;
  }
  if (anyLevel) {
    cache.levels = levels;
    cache.size = total;
  }

  // Guarded on the request count, not on truthiness. The old `if (hits && misses)` is false
  // when hits is 0 -- precisely the case an operator most needs a hit rate for, a cache being
  // asked and never answering. Absent when there were no requests at all, because an idle mount
  // having served none is a different fact from a hit rate of zero.
  const requests = (cache.hits ?? 0) + (cache.misses ?? 0);
  if (requests > 0) {
    cache.hitRate = (cache.hits ?? 0) / requests;
  }

  return cache;
}

/**
 * Operation counts and latency from `objectfs_operation*`. Null when nothing is recorded.
 *
 * `objectfs_operations_total` is labelled by operation and status, so a total is a sum across
 * samples; reading a single sample would report whichever operation came last. Average latency
 * is the histogram's `_sum` over its count -- a mean across the mount's whole life, not a recent
 * window, since a rate needs two scrapes and this has one.
 */
export function extractOperationStats(data: RawMetrics): OperationMetrics | null {
  const byOperation: Record<string, OperationDetail> = {};
  let total = 0;
  let successful = 0;
  let failed = 0;

  for (const sample of samplesFor(data, 'objectfs_operations_total')) {
    const operation = sample.labels['operation'] ?? '';
    total += sample.value;
    if (sample.labels['status'] === 'success') successful += sample.value;
    else if (sample.labels['status'] === 'error') failed += sample.value;

    const entry = byOperation[operation] ?? { count: 0 };
    entry.count += sample.value;
    byOperation[operation] = entry;
  }

  if (Object.keys(byOperation).length === 0) {
    return null;
  }

  for (const sample of samplesFor(data, 'objectfs_operation_duration_seconds_sum')) {
    const entry = byOperation[sample.labels['operation'] ?? ''];
    if (entry) {
      entry.durationSeconds = sample.value;
      if (entry.count > 0) entry.avgDurationSeconds = sample.value / entry.count;
    }
  }

  for (const sample of samplesFor(data, 'objectfs_operation_size_bytes_sum')) {
    const entry = byOperation[sample.labels['operation'] ?? ''];
    if (entry) entry.bytes = sample.value;
  }

  return { total, successful, failed, byOperation };
}

/**
 * Read and write volume, projected from the operation metrics.
 *
 * The `objectfs_io_*` names this used to read were invented. Reads and writes are carried by
 * `objectfs_operations_total` and `objectfs_operation_size_bytes_sum`, both labelled by
 * operation -- and as of v0.10.1 the FUSE layer does not record either through
 * RecordOperation, so against a live mount this is `{}`. That is the honest answer, and it
 * fills in on its own when the recording lands.
 */
export function extractIOStats(data: RawMetrics): IOMetrics {
  const operations = extractOperationStats(data);
  const io: IOMetrics = {};
  if (!operations) return io;

  const read = operations.byOperation['read'];
  if (read) {
    io.readOperations = read.count;
    if (read.bytes !== undefined) io.readBytes = read.bytes;
  }

  const write = operations.byOperation['write'];
  if (write) {
    io.writeOperations = write.count;
    if (write.bytes !== undefined) io.writeBytes = write.bytes;
  }

  return io;
}

/**
 * Errors from `objectfs_errors_total{operation,type}`. Null when there are none.
 *
 * The collector classifies each error as timeout, connection, not_found, permission, throttling
 * or other, and that split is kept rather than summed: a mount failing on permissions and one
 * failing on throttling call for different responses.
 */
export function extractErrorStats(data: RawMetrics): ErrorMetrics | null {
  const byType: Record<string, number> = {};
  const byOperation: Record<string, number> = {};
  let total = 0;
  let any = false;

  for (const sample of samplesFor(data, 'objectfs_errors_total')) {
    any = true;
    total += sample.value;

    const type = sample.labels['type'] ?? '';
    byType[type] = (byType[type] ?? 0) + sample.value;

    const operation = sample.labels['operation'] ?? '';
    byOperation[operation] = (byOperation[operation] ?? 0) + sample.value;
  }

  return any ? { total, byType, byOperation } : null;
}

/** `objectfs_active_connections`, an unlabelled gauge. Null when absent. */
export function extractConnectionStats(data: RawMetrics): ConnectionMetrics | null {
  const first = samplesFor(data, 'objectfs_active_connections')[0];

  return first ? { active: first.value } : null;
}
