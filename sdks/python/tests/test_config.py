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

``PresetFixturesMatchWhatTheSDKEmitsTest`` is the producing half of a cross-language contract, and it
is the reason #385 was found to be sixteen keys rather than the three it names. Each preset's
``to_yaml()`` is committed under ``sdks/testdata/presets/``, and ``TestSDKPresetsLoadUnderTheGoLoader``
in ``internal/config`` reads that directory by glob and puts every file through ``LoadFromFile`` and
``Validate``. The Go loader is the authority on what a config document may contain, so the assertion
has to be made by the Go loader; asserting the emitted keys against a list written here would only pin
the list, and the list is what drifted.

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

FIXTURE_DIR = os.path.join(
    os.path.dirname(__file__), '..', '..', 'testdata', 'presets'
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
        # Merged from the production preset rather than from Configuration(), because the sibling
        # assertion below only means something if a sibling holds a *non-default* value. A one-level
        # merge replaces the whole s3 section and S3Config's dataclass defaults then supply what was
        # lost -- so a sibling that already equals its default survives a broken merge and asserting
        # on it proves nothing. That is exactly how the equivalent bug went unnoticed in the
        # JavaScript SDK. `use_acceleration` is True in this preset and False by default, so it can
        # tell the two apart. (It used to be asserted on `max_retries` and `s3.timeout`, both at
        # their defaults; `timeout` was then removed in #385 for not existing in the Go schema.)
        base = Configuration.from_preset('production')
        self.assertTrue(base.storage.s3.use_acceleration)

        merged = base.merge({'storage': {'s3': {'region': 'eu-west-1'}}})
        self.assertEqual(merged.storage.s3.region, 'eu-west-1')
        self.assertTrue(merged.storage.s3.use_acceleration)
        self.assertEqual(merged.storage.s3.max_retries, base.storage.s3.max_retries)

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


class PresetFixturesMatchWhatTheSDKEmitsTest(unittest.TestCase):
    """Keep ``sdks/testdata/presets/`` equal to what this SDK writes today.

    Compare by default, write only under ``OBJECTFS_UPDATE_FIXTURES=1``. That asymmetry is the whole
    value. A test that simply wrote the files would make the Go-side gate assert against whatever was
    committed last: edit ``config.py``, do not run pytest, and ``TestSDKPresetsLoadUnderTheGoLoader``
    stays green on stale documents while ``save_to_file`` emits a key the daemon refuses. Comparing
    means the drift fails *here*, in the language that caused it, in a job that already runs.

    It is the same arrangement as ``sdks/testdata/metrics-scrape.txt`` in the other direction -- Go's
    ``TestSDKFixtureMatchesTheLiveScrape`` regenerates the scrape and compares rather than writing it,
    and takes ``-update-fixture`` to write. Regenerate here with::

        cd sdks/python && OBJECTFS_UPDATE_FIXTURES=1 pytest tests/test_config.py

    and commit what it writes, so a reviewer sees in the diff what the SDK now emits.

    Deliberately a loop over ``PRESETS``, so a preset added later is covered without editing the Go
    side -- that test globs the directory.
    """

    def _documents(self):
        """Every (filename, YAML) pair the SDK is expected to be able to emit.

        ``default.yaml`` is here as well as the five presets because each preset is a mutation of
        ``Configuration()``: a key that is wrong in the defaults is wrong in all five, and emitting the
        unmodified object means a Go-side failure points at the dataclass rather than at a preset.
        """
        documents = [('default.yaml', Configuration())]
        documents += [(f'{name}.yaml', Configuration.from_preset(name)) for name in PRESETS]

        for filename, config in documents:
            # validate() first: a document the SDK itself rejects is not worth asking the Go loader
            # about, and the failure should name the SDK rather than arrive as a confusing Go-side
            # error about a file that should never have been written.
            config.validate()
            yield filename, config.to_yaml()

    def test_fixtures_are_current(self):
        updating = os.environ.get('OBJECTFS_UPDATE_FIXTURES') == '1'

        if updating:
            os.makedirs(FIXTURE_DIR, exist_ok=True)

        for filename, emitted in self._documents():
            with self.subTest(fixture=filename):
                path = os.path.join(FIXTURE_DIR, filename)

                if updating:
                    with open(path, 'w', encoding='utf-8') as handle:
                        handle.write(emitted)
                    continue

                self.assertTrue(
                    os.path.exists(path),
                    f"{path} does not exist. internal/config's TestSDKPresetsLoadUnderTheGoLoader "
                    f"checks these documents against the Go loader, and a missing file is a check "
                    f"that does not run. Regenerate with "
                    f"OBJECTFS_UPDATE_FIXTURES=1 pytest tests/test_config.py",
                )

                with open(path, 'r', encoding='utf-8') as handle:
                    committed = handle.read()

                self.assertEqual(
                    emitted,
                    committed,
                    f"{filename} no longer matches what this SDK emits. The Go loader is checked "
                    f"against the committed file, so leaving it stale means a config key the daemon "
                    f"refuses can ship green (#385). If the change is intended, regenerate with "
                    f"OBJECTFS_UPDATE_FIXTURES=1 pytest tests/test_config.py and run "
                    f"go test ./internal/config/ -run TestSDKPresets",
                )


if __name__ == '__main__':
    unittest.main()
