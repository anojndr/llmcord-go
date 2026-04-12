# AGENTS.md

A dedicated guide for AI coding agents working on this Go project.

## Tooling Setup (golangci-lint)

**Use the official recommended binary installation method** (do **not** use `go install`):

```bash
curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b $(go env GOPATH)/bin latest
```

Verify installation:
```bash
golangci-lint --version
```

## Quality Gates (Mandatory After Every Change)

These quality gates **must** run and pass **after EVERY change** (feature, bugfix, refactor, or any edit). The agent must enforce them strictly before considering any task complete.

1. **Linting with golangci-lint**
   - Run: `golangci-lint run --default=all`
   - **Fix ALL issues reported by the linter** — no exceptions.
   - Never suppress, disable, or ignore any findings (no `//nolint` comments, no config exceptions, no rule disables). Always refactor the code to satisfy every enabled rule.

2. **Testing**
   - **Create or update tests for every change** (new code, modified behavior, bugfix — no exceptions).
   - Run the full test suite with race detection: `go test ./... -race -count=1`
   - All tests must pass cleanly with zero data races detected.

3. **Project Maintenance**
   - Always update `README.md` to reflect any changes (new features, setup steps, usage notes, etc.).
   - Always update `.gitignore` (see policy below).

## .gitignore Policy

`.gitignore` **acts as a whitelist**, not a blacklist (inspired by https://rgbcu.be/blog/gitignore).

- Default to ignoring everything: `*`
- Explicitly un-ignore only the files and directories that belong in the repository using `!` patterns.
- Keep it clean, future-proof, and minimal.

Example starting structure:
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

- Lint (strict): `golangci-lint run --default=all`
- Test with race detection: `go test ./... -race -count=1`
- Tidy dependencies: `go mod tidy`
- Format (after lint fixes): `gofmt -w .` or `gofumpt -w .`

## Testing Instructions

- Every single code change requires corresponding tests (use table-driven tests where appropriate).
- Run the full suite with `-race` after any edit.
- Fix any failing tests or races before moving on.

## Change / Pull Request Guidelines

- Run the complete quality gates locally before committing or submitting changes.
- Ensure `.gitignore` and `README.md` are always updated.
- All gates must pass cleanly.

Treat this AGENTS.md as living, authoritative documentation. Update it only if project conventions evolve. The agent must follow these rules exactly and never deviate.
