// Flat config, because eslint 10 reads nothing else.
//
// The rules here are a port of the `eslintConfig` block that used to live in package.json, not a
// rewrite — same five rules, same severities. That block is gone: eslint 10 ignores
// `package.json`'s `eslintConfig` entirely, so leaving it would have left a second set of rules
// that looks authoritative and is read by nothing. That is the failure mode #327 had just
// finished fixing in the other direction, where the block was read but named a config that did
// not resolve (`@typescript-eslint/recommended` rather than `plugin:@typescript-eslint/recommended`),
// so `npm run lint` errored out before linting a line and nobody noticed because no job ran it.
//
// Three things about the port are worth stating, because none of them is obvious from the diff:
//
//   - `eslint:recommended` becomes `js.configs.recommended`, and `plugin:@typescript-eslint/
//     recommended` becomes the spread of `tseslint.configs.recommended`. The string forms are not
//     accepted in flat config at all; there is no `extends` key to put them in.
//   - `env: { node: true, es6: true }` has no flat-config equivalent. It was two things at once —
//     global variables and a parser ecosystem version — and they separate here into
//     `languageOptions.globals` and `ecmaVersion`. `globals.node` is why `process` and `console`
//     are not `no-undef` errors; `sourceType: 'commonjs'` matches the `module: commonjs` in
//     tsconfig.json.
//   - the ignores block is required. eslint 10 no longer reads `.eslintignore`, and without
//     `dist/**` here the emitted JavaScript gets linted as source, which produces hundreds of
//     errors in generated code.
//
// Test files are linted, even though tsconfig.json excludes them from the build. Nothing here is
// type-aware — no `projectService`, no `parserOptions.project` — so the linter never needs to
// resolve jest globals or a second tsconfig, and the usual reason to exclude tests does not
// apply. Linting them found a real thing: `index.test.ts` carried an
// `eslint-disable-next-line @typescript-eslint/no-var-requires`, and typescript-eslint 8 renamed
// that rule to `no-require-imports`, so the directive was suppressing nothing.

const js = require('@eslint/js');
const tseslint = require('typescript-eslint');
const prettier = require('eslint-plugin-prettier');
const prettierConfig = require('eslint-config-prettier');
const globals = require('globals');

module.exports = [
  {
    ignores: ['dist/**', 'node_modules/**', 'coverage/**'],
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  // After tseslint.configs.recommended, not before: eslint-config-prettier's only job is to turn
  // off the stylistic rules those presets enable, so anything spread after it re-enables them and
  // the formatter and the linter start disagreeing about the same line.
  prettierConfig,
  {
    files: ['src/**/*.ts'],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'commonjs',
      globals: {
        ...globals.node,
      },
    },
    plugins: {
      prettier,
    },
    rules: {
      'prettier/prettier': 'error',
      '@typescript-eslint/no-unused-vars': [
        'error',
        { argsIgnorePattern: '^_', caughtErrorsIgnorePattern: '^_' },
      ],
      '@typescript-eslint/explicit-function-return-type': 'off',
      '@typescript-eslint/explicit-module-boundary-types': 'off',
      '@typescript-eslint/no-explicit-any': 'warn',
    },
  },
];
