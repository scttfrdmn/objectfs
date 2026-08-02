"""Tests for the Prometheus scrape parsing in objectfs.monitoring.

Every test here reads ``sdks/testdata/metrics-scrape.txt``, which is not a hand-written
sample: it is a real /metrics response captured from internal/metrics.Collector, and
``TestSDKFixtureMatchesTheLiveScrape`` in internal/metrics regenerates and compares it on
every Go test run. So a metric renamed in Go fails the Go suite, and the corrected fixture
then fails these tests -- which is the whole point.

The absence of that link is why the code these tests cover was broken. The parser split each
line on its first space, which fuses a metric's name to its label block; the extractors then
looked up names like ``cache_hits`` and ``objectfs_io_read_operations_total`` that ObjectFS
has never exported. Both halves were wrong, and each hid the other: fixing the parser alone
would still have found nothing, and fixing the names alone would still have had nothing to
look them up in. Neither could be noticed without comparing against a real scrape.
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..'))

from objectfs.monitoring import MetricsCollector  # noqa: E402

FIXTURE = os.path.join(
    os.path.dirname(__file__), '..', '..', 'testdata', 'metrics-scrape.txt'
)


def load_fixture() -> str:
    with open(FIXTURE, 'r', encoding='utf-8') as handle:
        return handle.read()


class ParseSampleLineTest(unittest.TestCase):
    """The line-level parse, which is where the original defect lived."""

    def setUp(self):
        self.collector = MetricsCollector()

    def test_labelled_line_separates_name_from_labels(self):
        name, labels, value = self.collector._parse_sample_line(
            'objectfs_cache_requests_total{service="objectfs",type="hit"} 3'
        )

        # The old first-space split produced
        # 'objectfs_cache_requests_total{service="objectfs",type="hit"}' as the name, which
        # no lookup anywhere in the SDK could ever match.
        self.assertEqual(name, 'objectfs_cache_requests_total')
        self.assertEqual(labels, {'service': 'objectfs', 'type': 'hit'})
        self.assertEqual(value, 3.0)

    def test_unlabelled_line(self):
        name, labels, value = self.collector._parse_sample_line('some_metric 42')

        self.assertEqual(name, 'some_metric')
        self.assertEqual(labels, {})
        self.assertEqual(value, 42.0)

    def test_scientific_notation(self):
        # Prometheus writes large float values this way, and cache_size_bytes is a float
        # gauge -- 1 MiB is exported as 1.048576e+06, not 1048576.
        _, _, value = self.collector._parse_sample_line(
            'objectfs_cache_size_bytes{level="L1"} 1.048576e+06'
        )

        self.assertEqual(value, 1048576.0)

    def test_positive_infinity_bucket(self):
        # The +Inf bucket is a normal sample line and float('+Inf') accepts it. A parser
        # that rejected it would drop the histogram's total count.
        name, labels, value = self.collector._parse_sample_line(
            'objectfs_operation_duration_seconds_bucket{operation="read",le="+Inf"} 2'
        )

        self.assertEqual(name, 'objectfs_operation_duration_seconds_bucket')
        self.assertEqual(labels['le'], '+Inf')
        self.assertEqual(value, 2.0)

    def test_comma_inside_a_label_value(self):
        # A label value is an escaped string, so a comma in it is data. Splitting the label
        # block on ',' would produce two broken fragments and lose the sample.
        _, labels, value = self.collector._parse_sample_line(
            'objectfs_errors_total{operation="read",type="a, b"} 1'
        )

        self.assertEqual(labels, {'operation': 'read', 'type': 'a, b'})
        self.assertEqual(value, 1.0)

    def test_escaped_quote_inside_a_label_value(self):
        _, labels, _ = self.collector._parse_sample_line(
            'objectfs_errors_total{type="say \\"hi\\""} 1'
        )

        self.assertEqual(labels, {'type': 'say "hi"'})

    def test_brace_inside_a_label_value(self):
        # rfind('}') rather than find('}'), so a closing brace in a value does not truncate
        # the label block.
        name, labels, value = self.collector._parse_sample_line(
            'objectfs_errors_total{type="a}b",operation="read"} 7'
        )

        self.assertEqual(name, 'objectfs_errors_total')
        self.assertEqual(labels, {'type': 'a}b', 'operation': 'read'})
        self.assertEqual(value, 7.0)

    def test_trailing_timestamp_is_not_the_value(self):
        # The exposition format permits a trailing timestamp. Reading it as the value would
        # report a millisecond epoch as a metric.
        _, _, value = self.collector._parse_sample_line('some_metric 5 1698000000000')

        self.assertEqual(value, 5.0)

    def test_non_sample_lines_are_rejected(self):
        for line in (
            '# HELP objectfs_operations_total Total number of operations',
            '# TYPE objectfs_operations_total counter',
            'no_value_at_all',
            'bad_value not_a_number',
            'objectfs_thing{unterminated="x" 5',
        ):
            name, _, _ = self.collector._parse_sample_line(line)
            self.assertIsNone(name, f'{line!r} should not parse as a sample')


class ParseScrapeTest(unittest.TestCase):
    """The whole-document parse, against a real captured scrape."""

    def setUp(self):
        self.collector = MetricsCollector()
        self.parsed = self.collector._parse_prometheus_metrics(load_fixture())

    def test_comments_are_skipped_and_samples_are_found(self):
        samples = self.parsed['samples']

        self.assertTrue(samples, 'the fixture parsed to no samples at all')
        for sample in samples:
            self.assertFalse(sample['name'].startswith('#'))
            self.assertNotIn('{', sample['name'])

    def test_repeated_names_are_all_kept(self):
        # hit and miss are two samples of one name. A dict keyed by name would keep one.
        names = [
            s['name'] for s in self.parsed['samples']
            if s['name'] == 'objectfs_cache_requests_total'
        ]

        self.assertEqual(len(names), 2)

    def test_every_family_the_collector_exports_is_present(self):
        # Enumerated from internal/metrics.initMetrics. This is the SDK's half of the
        # contract that internal/metrics.TestExportedNamesAreTheOnesDocumentedAndScraped
        # asserts from the Go side.
        found = {s['name'] for s in self.parsed['samples']}

        for name in (
            'objectfs_operations_total',
            'objectfs_operation_duration_seconds_sum',
            'objectfs_operation_size_bytes_sum',
            'objectfs_cache_requests_total',
            'objectfs_cache_size_bytes',
            'objectfs_active_connections',
            'objectfs_errors_total',
        ):
            self.assertIn(name, found)

    def test_operator_labels_survive_the_parse(self):
        # monitoring.metrics.custom_labels attaches these to every series. An SDK that
        # dropped them could not tell two nodes' metrics apart.
        for sample in self.parsed['samples']:
            self.assertEqual(sample['labels'].get('service'), 'objectfs')


class ExtractCacheStatsTest(unittest.TestCase):
    """The hit rate -- the number the README example prints."""

    def setUp(self):
        self.collector = MetricsCollector()
        self.raw = self.collector._parse_prometheus_metrics(load_fixture())
        self.stats = self.collector._extract_cache_stats(self.raw)

    def test_hits_and_misses_come_from_the_type_label(self):
        self.assertEqual(self.stats['hits'], 3.0)
        self.assertEqual(self.stats['misses'], 1.0)

    def test_hit_rate_is_derived(self):
        # 3 hits, 1 miss. Deliberately not 0.0, 0.5 or 1.0: a broken parser can produce
        # those by accident, and 0.75 requires having read both labelled samples correctly.
        self.assertAlmostEqual(self.stats['hit_rate'], 0.75)

    def test_per_level_sizes(self):
        self.assertEqual(self.stats['levels'], {'L1': 1048576.0, 'L2': 5242880.0})
        self.assertEqual(self.stats['size'], 1048576.0 + 5242880.0)

    def test_no_cache_requests_yields_no_hit_rate(self):
        # An idle mount has served no cache request, and 'no requests' is a different fact
        # from 'a hit rate of zero'. Reporting 0.0 would look like a cache that never hits.
        empty = self.collector._extract_cache_stats({'samples': []})

        self.assertNotIn('hit_rate', empty)
        self.assertNotIn('hits', empty)

    def test_misses_only_still_gives_a_hit_rate_of_zero(self):
        # This was the live state of v0.10.0: RecordCacheHit had no caller anywhere in the
        # repo, so only misses were ever recorded. A hit rate of 0.0 is the correct reading
        # of that scrape, and it is exactly what an operator should have seen.
        stats = self.collector._extract_cache_stats({
            'samples': [{
                'name': 'objectfs_cache_requests_total',
                'labels': {'type': 'miss'},
                'value': 9.0,
            }]
        })

        self.assertEqual(stats['hit_rate'], 0.0)
        self.assertEqual(stats['misses'], 9.0)


class ExtractOperationStatsTest(unittest.TestCase):

    def setUp(self):
        self.collector = MetricsCollector()
        self.raw = self.collector._parse_prometheus_metrics(load_fixture())
        self.stats = self.collector._extract_operation_stats(self.raw)

    def test_totals_sum_across_labels(self):
        # The fixture records read/success, read/error and write/success. A reader that took
        # one sample's value would report 1 for every field.
        self.assertEqual(self.stats['total'], 3.0)
        self.assertEqual(self.stats['successful'], 2.0)
        self.assertEqual(self.stats['failed'], 1.0)

    def test_per_operation_breakdown(self):
        self.assertEqual(self.stats['by_operation']['read']['count'], 2.0)
        self.assertEqual(self.stats['by_operation']['write']['count'], 1.0)

    def test_average_duration_from_the_histogram(self):
        # Two reads at 12 ms and 9 ms: sum 0.021 s over 2 observations.
        read = self.stats['by_operation']['read']

        self.assertAlmostEqual(read['duration_seconds'], 0.021, places=6)
        self.assertAlmostEqual(read['avg_duration_seconds'], 0.0105, places=6)

    def test_byte_totals(self):
        self.assertEqual(self.stats['by_operation']['read']['bytes'], 8192.0)
        self.assertEqual(self.stats['by_operation']['write']['bytes'], 8192.0)

    def test_empty_scrape_yields_nothing(self):
        self.assertEqual(self.collector._extract_operation_stats({'samples': []}), {})


class ExtractIOStatsTest(unittest.TestCase):

    def setUp(self):
        self.collector = MetricsCollector()
        self.raw = self.collector._parse_prometheus_metrics(load_fixture())

    def test_projects_read_and_write_operations(self):
        stats = self.collector._extract_io_stats(self.raw)

        self.assertEqual(stats['read_operations'], 2.0)
        self.assertEqual(stats['write_bytes'], 8192.0)

    def test_operations_not_recorded_are_absent_not_zero(self):
        # As of v0.10.1 the FUSE layer does not call RecordOperation for reads and writes, so
        # a real mount's scrape has no read/write samples and this is {}. Returning zeros
        # would report an idle filesystem instead of an unrecorded one.
        stats = self.collector._extract_io_stats({
            'samples': [{
                'name': 'objectfs_operations_total',
                'labels': {'operation': 'prefetch', 'status': 'success'},
                'value': 4.0,
            }]
        })

        self.assertEqual(stats, {})


class ExtractErrorStatsTest(unittest.TestCase):

    def setUp(self):
        self.collector = MetricsCollector()
        self.raw = self.collector._parse_prometheus_metrics(load_fixture())
        self.stats = self.collector._extract_error_stats(self.raw)

    def test_split_by_classification(self):
        # The fixture's one RecordError call produces a 'timeout' classification, and the
        # failed RecordOperation produces a 'failure'. Both are errors_total samples.
        self.assertEqual(self.stats['total'], 2.0)
        self.assertEqual(self.stats['by_type']['timeout'], 1.0)
        self.assertEqual(self.stats['by_type']['failure'], 1.0)

    def test_summed_per_operation(self):
        self.assertEqual(self.stats['by_operation']['read'], 2.0)

    def test_no_errors_yields_nothing(self):
        self.assertEqual(self.collector._extract_error_stats({'samples': []}), {})


class ExtractConnectionStatsTest(unittest.TestCase):

    def test_unlabelled_gauge(self):
        collector = MetricsCollector()
        raw = collector._parse_prometheus_metrics(load_fixture())

        self.assertEqual(collector._extract_connection_stats(raw), {'active': 4.0})

    def test_absent_gauge(self):
        collector = MetricsCollector()

        self.assertEqual(collector._extract_connection_stats({'samples': []}), {})


class ProcessMetricsTest(unittest.TestCase):
    """The shape get_metrics() returns, which is the SDK's public surface."""

    def setUp(self):
        self.collector = MetricsCollector()
        raw = self.collector._parse_prometheus_metrics(load_fixture())
        self.processed = self.collector._process_metrics(raw)

    def test_readme_example_works(self):
        # sdks/python/README.md prints metrics['cache']['hit_rate']. That raised KeyError
        # against a healthy mount for the whole life of the SDK.
        self.assertAlmostEqual(self.processed['cache']['hit_rate'], 0.75)

    def test_sections_present(self):
        for section in ('cache', 'io', 'operations', 'errors', 'connections'):
            self.assertIn(section, self.processed)

    def test_raw_samples_are_kept(self):
        # So a caller can read a metric the extractors do not surface without re-scraping.
        self.assertTrue(self.processed['raw']['samples'])

    def test_no_invented_sections(self):
        # network, storage and distributed were built from metric names ObjectFS has never
        # exported, so each was permanently {} while advertising a whole subsystem's
        # telemetry.
        for absent in ('network', 'storage', 'distributed'):
            self.assertNotIn(absent, self.processed)


if __name__ == '__main__':
    unittest.main()
