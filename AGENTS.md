# AGENTS.md

A dedicated guide for AI coding agents working on this Go project.

## Quality Gates (Mandatory After Every Change)

**These quality gates must run after EVERY change (feature, bugfix, refactor, or edit). The agent must enforce them strictly before considering any task complete.**

1. **Linting with golangci-lint**
   - Run: `golangci-lint run --default=all`
   - **Fix ALL issues reported by the linter**.
   - Never suppress, disable, or ignore any findings (no `//nolint` directives, no config exceptions, no rule disables). Always refactor the code to fully satisfy every rule.

2. **Testing**
   - **Create or update tests for every change** (new code, modified behavior, or bugfix — no exceptions).
   - Run the full test suite with race detection: `go test ./... -race -count=1`
   - All tests must pass cleanly with zero data races detected.

3. **Project Maintenance**
   - Always update `README.md` to reflect any changes (new features, setup steps, usage notes, etc.).
   - Always update `.gitignore` (see policy below).

## .gitignore Policy

`.gitignore` **acts as a whitelist**, not a blacklist (inspired by https://rgbcu.be/blog/gitignore).

- Default to ignoring everything: `*`
- Explicitly un-ignore only the files and directories that belong in the repository using `!` patterns.
- Keep it clean, future-proof, and minimal. Example starting structure:
  ```
  *
  !.gitignore
  !README.md
  !go.mod
  !go.sum
  !cmd/
  !cmd/**/*
  !internal/
  !internal/**/*
  !pkg/
  !pkg/**/*
  !.github/
  !.github/**/*
  !docs/
  !docs/**/*
  ```

## Development Commands

- Install/update tools: `go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest`
- Lint: `golangci-lint run --default=all`
- Test with race detection: `go test ./... -race`
- Tidy dependencies: `go mod tidy`
- Format: `gofmt -w .` or `gofumpt -w .` (after lint fixes)

## Testing Instructions

- Every single code change requires corresponding tests.
- Use table-driven tests where appropriate.
- Run the full suite with `-race` after any edit.
- Fix any failing tests or races before moving on.

## Pull Request / Change Guidelines

- Run the complete quality gates locally before committing or submitting changes.
- Ensure `.gitignore` and `README.md` are updated.
- All gates must pass cleanly.

Treat this AGENTS.md as living, authoritative documentation. Update it only if project conventions evolve. The agent must follow these rules exactly and never deviate.