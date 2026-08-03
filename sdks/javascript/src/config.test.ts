/**
 * Tests for Configuration's defaulting and merge in ./config.
 *
 * These exist because `tsc` was reporting the bug and nothing was running `tsc`. Eleven TS2739
 * "is missing the following properties" errors pointed at the preset table, and the reason a
 * partial preset did not typecheck was that the constructor could not merge one: it spread
 *
 *     { s3: { ...defaults, ...config?.storage?.s3 }, ...config?.storage }
 *
 * so the caller's whole `storage` object landed last and replaced the merged `s3` wholesale.
 * `fromPreset('development')`, which sets `storage.s3.region` and nothing else, produced an
 * S3Config containing only `region` — no maxRetries, no timeout, no costOptimization — and
 * `fromPreset('cost-optimized')`, which sets only `costOptimization`, produced one with no region,
 * so `validate()` rejected a preset this SDK ships.
 *
 * The assertions below are about *defaults surviving an override*, at every nesting depth the
 * config has. A one-level spread passes any test that only checks the override took effect, which
 * is why the checks come in pairs: the override applied, AND its siblings are still there.
 *
 * Run with the repo's configured runner:
 *
 *     cd sdks/javascript && npm install && npm test
 *
 * node:assert rather than jest's `expect`, matching ./prometheus.test.ts: jest supplies the
 * harness, and nothing here depends on it.
 */

import { describe, test } from '@jest/globals';
import assert from 'node:assert/strict';

import { Configuration } from './config';
import { ConfigurationError } from './errors';

describe('defaults', () => {
  test('a bare Configuration is fully populated', () => {
    const c = new Configuration();
    assert.equal(c.storage.s3.region, 'us-east-1');
    assert.equal(c.storage.s3.maxRetries, 3);
    assert.equal(c.storage.s3.timeout, 30);
    assert.equal(c.storage.s3.costOptimization.transitionToIA, 30);
    assert.equal(c.performance.readAheadSize, '4MB');
    assert.equal(c.cluster.listenAddr, '0.0.0.0:8080');
    assert.equal(c.security.authMethod, 'none');
    assert.equal(c.monitoring.opentelemetry.endpoint, 'localhost:4317');
    assert.equal(c.fuse.umask, 0o022);
  });

  test('a one-key override at depth 3 keeps its siblings', () => {
    // storage.s3.costOptimization.enabled is the deepest leaf the config has. Before the fix this
    // override erased region, maxRetries, timeout, and the four sibling costOptimization fields.
    const c = new Configuration({
      storage: { s3: { costOptimization: { enabled: false } } },
    });
    assert.equal(c.storage.s3.costOptimization.enabled, false);
    assert.equal(c.storage.s3.costOptimization.tieringEnabled, true);
    assert.equal(c.storage.s3.costOptimization.transitionToGlacier, 90);
    assert.equal(c.storage.s3.region, 'us-east-1');
    assert.equal(c.storage.s3.maxRetries, 3);
  });

  test('an explicit empty array replaces rather than merges', () => {
    // seedNodes defaults to [] so this is only observable in the other direction: a caller who
    // supplies nodes gets exactly those, not those plus anything.
    const c = new Configuration({ cluster: { seedNodes: ['a:8080'] } });
    assert.deepEqual(c.cluster.seedNodes, ['a:8080']);
    assert.equal(c.cluster.replicationFactor, 3);
  });
});

describe('fromPreset', () => {
  test('development keeps every default it does not name', () => {
    const c = Configuration.fromPreset('development');
    assert.equal(c.performance.cacheSize, '1GB'); // named by the preset
    assert.equal(c.performance.maxConcurrency, 50); // named by the preset
    assert.equal(c.performance.readAheadSize, '4MB'); // must survive
    assert.equal(c.performance.writeBufferSize, '4MB');
    assert.equal(c.storage.s3.region, 'us-east-1');
    assert.equal(c.storage.s3.maxRetries, 3);
    assert.equal(c.storage.s3.timeout, 30);
    assert.equal(c.storage.s3.costOptimization.transitionToIA, 30);
    assert.equal(c.global.logLevel, 'DEBUG');
    assert.equal(c.global.daemon, false);
  });

  test('cost-optimized still has a region, and validates', () => {
    // The regression this file exists for. cost-optimized sets storage.s3.costOptimization only,
    // so the old merge left s3.region undefined and validate() threw "S3 region is required" on a
    // preset the SDK ships.
    const c = Configuration.fromPreset('cost-optimized');
    assert.equal(c.storage.s3.region, 'us-east-1');
    assert.equal(c.storage.s3.costOptimization.transitionToIA, 7);
    assert.equal(c.storage.s3.costOptimization.transitionToGlacier, 30);
    assert.doesNotThrow(() => c.validate());
  });

  test('production merges a nested partial without dropping siblings', () => {
    const c = Configuration.fromPreset('production');
    assert.equal(c.storage.s3.useAcceleration, true);
    assert.equal(c.storage.s3.costOptimization.enabled, true); // named
    assert.equal(c.storage.s3.costOptimization.transitionToGlacier, 90); // default, must survive
    assert.equal(c.monitoring.enabled, true); // named
    assert.equal(c.monitoring.metricsAddr, ':9090'); // default, must survive
    assert.equal(c.performance.multilevelCaching, true);
    assert.equal(c.performance.maxWriteBuffer, '64MB');
  });

  test('every shipped preset validates', () => {
    // The cheapest possible guard against the class: a preset that cannot pass its own validate()
    // is not usable, and before the merge fix two of these five could not.
    for (const preset of [
      'development',
      'production',
      'high-performance',
      'cost-optimized',
      'cluster',
    ] as const) {
      const c = Configuration.fromPreset(preset);
      assert.doesNotThrow(() => c.validate(), `${preset} should validate`);
      assert.ok(c.storage.s3.region, `${preset} should have a region`);
      assert.ok(
        c.performance.maxConcurrency > 0,
        `${preset} should have a concurrency`
      );
    }
  });

  test('cluster keeps the addresses it does not name', () => {
    const c = Configuration.fromPreset('cluster');
    assert.equal(c.cluster.enabled, true);
    assert.equal(c.cluster.consistencyLevel, 'strong');
    assert.equal(c.cluster.listenAddr, '0.0.0.0:8080'); // default, and validate() requires it
    assert.equal(c.cluster.heartbeatInterval, '1s');
    assert.equal(c.security.enabled, true);
    assert.equal(c.security.authMethod, 'none'); // default, must survive
  });

  test('cluster leaves TLS to the caller, who has the certificate paths', () => {
    // The preset used to set tlsEnabled: true, which validate() rejects without cert and key
    // paths — a preset cannot know either. Turning TLS on is a merge, and that merge validates.
    const c = Configuration.fromPreset('cluster');
    assert.equal(c.security.tlsEnabled, false);

    const withTls = c.merge({
      security: {
        tlsEnabled: true,
        tlsCertPath: '/etc/objectfs/tls.crt',
        tlsKeyPath: '/etc/objectfs/tls.key',
      },
    });
    assert.doesNotThrow(() => withTls.validate());

    // And the flag alone is still rejected, which is the check that made the preset unusable.
    assert.throws(
      () => c.merge({ security: { tlsEnabled: true } }).validate(),
      /TLS certificate/
    );
  });

  test('an unknown preset is a ConfigurationError', () => {
    assert.throws(
      () => Configuration.fromPreset('nonsense' as never),
      (e: unknown) => e instanceof ConfigurationError
    );
  });
});

describe('merge', () => {
  test('a nested override keeps unrelated defaults', () => {
    const c = new Configuration().merge({
      storage: { s3: { region: 'eu-west-1' } },
    });
    assert.equal(c.storage.s3.region, 'eu-west-1');
    assert.equal(c.storage.s3.maxRetries, 3);
    assert.equal(c.storage.s3.costOptimization.transitionToIA, 30);
  });

  test('merge does not mutate the receiver', () => {
    const base = new Configuration();
    const merged = base.merge({ storage: { s3: { region: 'ap-south-1' } } });
    assert.equal(merged.storage.s3.region, 'ap-south-1');
    assert.equal(base.storage.s3.region, 'us-east-1');
  });
});

describe('validate', () => {
  test('rejects a blank region', () => {
    const c = new Configuration({ storage: { s3: { region: '' } } });
    assert.throws(() => c.validate(), /region is required/);
  });

  test('rejects a non-positive concurrency', () => {
    const c = new Configuration({ performance: { maxConcurrency: 0 } });
    assert.throws(() => c.validate(), /maxConcurrency/);
  });

  test('rejects a cluster with no listen address', () => {
    const c = new Configuration({ cluster: { enabled: true, listenAddr: '' } });
    assert.throws(() => c.validate(), /listenAddr/);
  });
});
