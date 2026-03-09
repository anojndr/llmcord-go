# agents.md

## Purpose

This repository requires strict post-change quality enforcement for all Go code changes.

The agent must do only two things consistently after every change:

1. Enforce quality gates after each change
2. Create and maintain tests for every change

## Mandatory Rules

### 1. Quality gates after every change

After every code change, the agent must run all required quality gates and fix any failures before considering the task complete.

Minimum required gates after each change:

- `gofmt -s -w .`
- `go mod tidy`
- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `golangci-lint run --default=all`

The agent must fix all issues reported by these commands.

The agent must not leave lint findings, vet findings, formatting issues, failing tests, race warnings, or module drift unresolved.

### 2. Tests are required for every change

Every change must include tests.

Rules:

- Every behavior change must add or update automated tests
- Every bug fix must include a regression test
- Every new feature must include tests covering expected behavior
- Refactors must preserve or improve test coverage around changed behavior
- If a change affects concurrency, the tests must exercise concurrent behavior where practical
- If a change affects error handling, the tests must verify failure paths too

A change is not complete unless the relevant tests exist and pass.

## Linting Policy

`golangci-lint` is mandatory and must be run in all-linters mode for the installed v2 version.

Required command:

- `golangci-lint run --default=all`

The agent must fix all issues reported by `golangci-lint`.

The agent must not suppress, ignore, or bypass linter findings unless explicitly instructed by a human reviewer.

## Race Condition Testing

After every change, the agent must test for race conditions.

Required command:

- `go test -race ./...`

Any race detector failure must be treated as a blocking issue and must be fixed before the task is complete.

## Code Comment Policy

Do not write useless comments in the code.

Rules:

- Do not add comments that restate what the code already clearly says
- Do not add obvious line-by-line narration
- Prefer clear naming and simple structure over explanatory noise
- Add comments only when they explain intent, non-obvious constraints, reasoning, or important caveats

## Documentation and Repository Hygiene

The agent must always review whether the following files need updates:

- `README.md`
- `.gitignore`

### README.md

Always update `README.md` if necessary.

`README.md` must be updated if necessary whenever a change affects any of the following:

- setup
- usage
- commands
- configuration
- testing
- development workflow
- behavior visible to contributors or users

If a change does not affect documentation, do not modify `README.md` unnecessarily.

### .gitignore

Always update `.gitignore` if necessary.

`.gitignore` must always be kept up to date.

This repository treats `.gitignore` as a whitelist-oriented control file.

Inspired by https://rgbcu.be/blog/gitignore, prefer a restrictive approach where only intentionally tracked files and directories are allowed, instead of endlessly adding new junk patterns after the fact.

The agent must:

- review whether new files created by the change should be tracked or ignored
- update `.gitignore` if necessary when project structure changes
- preserve the whitelist philosophy where practical
- prevent editor files, build outputs, temporary files, caches, local scripts, and other accidental artifacts from being committed

## Completion Criteria

A change is complete only when all of the following are true:

- code is formatted
- dependencies are tidy
- tests were added or updated for the change
- `go test ./...` passes
- `go test -race ./...` passes
- `go vet ./...` passes
- `golangci-lint run --default=all` passes with all issues fixed
- `README.md` is updated if necessary
- `.gitignore` is updated if necessary

## Required Post-Change Command Checklist

Run these after every change:

- `gofmt -s -w .`
- `go mod tidy`
- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `golangci-lint run --default=all`

Do not mark work complete until every command passes and all reported issues are fixed.