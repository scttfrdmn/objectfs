"""Tests for objectfs.config.

The presets are the part that had a defect: ``from_preset('cluster')`` set
``security.tls_enabled = True``, and ``SecurityConfig.validate()`` requires certificate and key
paths whenever TLS is on -- paths a preset cannot know. So one of the five shipped presets could not
pass its own ``validate()``, and the SDK had no test that called both on the same object.
``EveryPresetValidatesTest`` is that test, and it is deliberately a loop over the preset names
rather than five separate cases, so a preset added later is covered by existing code.

The merge assertions come in pairs -- the override applied *and* its siblings survived. A one-level
merge passes any test that only checks the override, which is how the equivalent bug lived through
a release in the JavaScript SDK.

Run from ``sdks/python``::

    pip install . pytest && pytest tests/ -q
"""

import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..'))

from objectfs.config import Configuration  # noqa: E402
from objectfs.exceptions import ConfigurationError  # noqa: E402

PRESETS = (
    'development',
    'production',
    'high-performance',
    'cost-optimized',
    'cluster',
)


class EveryPresetValidatesTest(unittest.TestCase):
    """A preset that cannot pass validate() is unusable, and this is what finds it."""

    def test_all_presets_validate(self):
        for name in PRESETS:
            with self.subTest(preset=name):
                Configuration.from_preset(name).validate()

    def test_every_preset_keeps_a_region(self):
        """A preset naming one S3 field must not lose the rest of the section."""
        for name in PRESETS:
            with self.subTest(preset=name):
                self.assertTrue(Configuration.from_preset(name).storage.s3.region)

    def test_unknown_preset_names_itself(self):
        with self.assertRaises(ConfigurationError) as caught:
            Configuration.from_preset('no-such-preset')
        self.assertIn('no-such-preset', str(caught.exception))


class ClusterPresetLeavesTLSToTheCallerTest(unittest.TestCase):
    """Both directions of the fix, so neither can regress silently."""

    def test_preset_does_not_enable_tls(self):
        self.assertFalse(Configuration.from_preset('cluster').security.tls_enabled)

    def test_preset_still_enables_the_cluster(self):
        config = Configuration.from_preset('cluster')
        self.assertTrue(config.cluster.enabled)
        self.assertTrue(config.security.enabled)

    def test_caller_can_enable_tls_with_paths(self):
        config = Configuration.from_preset('cluster').merge({
            'security': {
                'tls_enabled': True,
                'tls_cert_path': '/etc/objectfs/tls.crt',
                'tls_key_path': '/etc/objectfs/tls.key',
            },
        })
        config.validate()
        self.assertTrue(config.security.tls_enabled)

    def test_enabling_tls_without_paths_is_still_rejected(self):
        config = Configuration.from_preset('cluster').merge({
            'security': {'tls_enabled': True},
        })
        with self.assertRaises(ConfigurationError):
            config.validate()


class MergeIsDeepTest(unittest.TestCase):
    """Each test asserts the override applied *and* that its siblings survived."""

    def test_nested_s3_field(self):
        merged = Configuration().merge({'storage': {'s3': {'region': 'eu-west-1'}}})
        self.assertEqual(merged.storage.s3.region, 'eu-west-1')
        # The sibling assertion. A one-level merge replaces the whole s3 section, and
        # S3Config's dataclass defaults would then have to supply these -- which is exactly why
        # the JavaScript equivalent went unnoticed: the dataclass hid the loss for most fields.
        default = Configuration()
        self.assertEqual(merged.storage.s3.max_retries, default.storage.s3.max_retries)
        self.assertEqual(merged.storage.s3.timeout, default.storage.s3.timeout)

    def test_untouched_sections_survive(self):
        default = Configuration()
        merged = default.merge({'performance': {'max_concurrency': 42}})
        self.assertEqual(merged.performance.max_concurrency, 42)
        self.assertEqual(merged.performance.cache_size, default.performance.cache_size)
        self.assertEqual(merged.global_config.log_level, default.global_config.log_level)
        self.assertEqual(merged.storage.s3.region, default.storage.s3.region)

    def test_merge_does_not_mutate_the_receiver(self):
        config = Configuration()
        original = config.storage.s3.region
        config.merge({'storage': {'s3': {'region': 'ap-south-1'}}})
        self.assertEqual(config.storage.s3.region, original)


class RoundTripTest(unittest.TestCase):
    """to_dict/from_dict and to_yaml/from_file must agree, or presets cannot be saved."""

    def test_dict_round_trip_preserves_every_preset(self):
        for name in PRESETS:
            with self.subTest(preset=name):
                config = Configuration.from_preset(name)
                self.assertEqual(
                    Configuration.from_dict(config.to_dict()).to_dict(),
                    config.to_dict(),
                )

    def test_yaml_file_round_trip(self):
        config = Configuration.from_preset('production')
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, 'objectfs.yaml')
            config.save_to_file(path)
            self.assertEqual(Configuration.from_file(path).to_dict(), config.to_dict())


if __name__ == '__main__':
    unittest.main()
