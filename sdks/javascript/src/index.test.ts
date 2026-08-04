/**
 * Tests for the package entry point.
 *
 * A consumer of a published package can only reach what ./index.ts re-exports, and nothing checked
 * that list against what the modules actually declare. Two things were wrong as a result:
 * `CacheError` — which `clearCache` and `warmCache` throw — was declared in ./errors and exported by
 * nothing, so a caller could not name it in a `catch`; and `LICENSE` said `'MIT'` while ObjectFS is
 * Apache-2.0, which is the SDK's own API giving a wrong answer to a compliance question.
 *
 * The errors test is deliberately written as "every class ./errors declares is re-exported" rather
 * than as a list of eleven names, so a class added later is covered by existing code. That is the
 * same reasoning as the loop over preset names in config.test.ts, which is what found the third
 * broken preset after a hand-written probe had found two.
 *
 * Run with the repo's configured runner:
 *
 *     cd sdks/javascript && npm install && npm test
 */

import { describe, test } from '@jest/globals';
import assert from 'node:assert/strict';

import * as index from './index';
import * as errors from './errors';

describe('the entry point', () => {
  test('re-exports every error class ./errors declares', () => {
    const declared = Object.keys(errors).filter(
      (name) => typeof (errors as Record<string, unknown>)[name] === 'function'
    );
    // Guard against the loop passing because the filter found nothing.
    assert.ok(
      declared.length >= 11,
      `expected ./errors to declare classes, found ${declared.length}`
    );

    const missing = declared.filter((name) => !(name in index));
    assert.deepEqual(
      missing,
      [],
      `declared in ./errors but not re-exported from the package: ${missing.join(
        ', '
      )}`
    );
  });

  test('exports the errors the client actually throws', () => {
    // Named explicitly as well as covered by the loop above, because these are load-bearing: a
    // caller who cannot import the class cannot distinguish this failure from any other.
    for (const name of [
      'CacheError',
      'DistributedError',
      'StorageError',
    ] as const) {
      assert.ok(
        name in index,
        `${name} is thrown by ./client but not exported`
      );
    }
  });

  test('the error classes are usable in instanceof after crossing the entry point', () => {
    // Re-exporting a name is not the same as re-exporting the same binding; if these ever diverged,
    // `catch (e) { if (e instanceof CacheError) }` would silently stop matching.
    //
    // Indexed rather than written as `index.CacheError` on purpose. A missing export is a TS2339,
    // which fails the *suite compile* and takes the two assertions above down with it — so the
    // failure message would name a type error rather than the missing export, and every other
    // assertion in this file would go unreported. Looked up dynamically, a missing export fails
    // here, as an assertion, saying which name is gone.
    const bag = index as Record<string, unknown>;
    for (const name of [
      'CacheError',
      'DistributedError',
      'ValidationError',
    ] as const) {
      assert.equal(
        bag[name],
        (errors as Record<string, unknown>)[name],
        `${name} is not the same binding`
      );
    }
    const CacheError = bag.CacheError as new (m: string) => Error;
    assert.ok(new CacheError('x') instanceof index.ObjectFSError);
  });

  test('LICENSE agrees with package.json', () => {
    // The rule name is `no-require-imports`, not `no-var-requires`: typescript-eslint 8 renamed
    // it, and the stale directive that used to be here silenced nothing while itself reporting
    // as an unused disable. require() is deliberate — an `import` of ../package.json would pull
    // the manifest into the compiled output's rootDir and change dist/'s shape, and this test
    // exists to read the manifest the package actually ships with.
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    const pkg = require('../package.json') as {
      license: string;
      version: string;
    };
    assert.equal(index.LICENSE, pkg.license);
    assert.equal(index.VERSION, pkg.version);
  });
});
