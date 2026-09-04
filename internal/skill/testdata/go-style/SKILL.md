---
name: go-style
description: Go style and idiom conventions for the Red Platform codebase. Covers formatting, naming, error handling, and idiomatic patterns.
---
# Go Style

## Formatting

- Run `gofmt` or `goimports` before committing
- Use tabs for indentation (gofmt standard)
- Maximum line length: 100 characters (soft), 120 characters (hard)

## Naming Conventions

### Functions and Methods
- camelCase for exported functions: `CalculateTotal()`
- camelCase for unexported functions: `calculateTotal()`
- Verbs for actions: `Compute`, `Process`, `Validate`

### Types and Structs
- PascalCase for exported types: `UserRepository`
- camelCase for unexported types: `userContext`
- Nouns or noun phrases for types

### Variables and Constants
- camelCase for variables: `maxRetries`
- UPPER_SNAKE_CASE for constants: `MaxRetries`
- Avoid single-character variable names except in loop scopes

### Files
- lowercase with optional suffix: `user.go`, `validation.go`
- Test files: `user_test.go`

## Error Handling

- Return errors as the last return value
- Use `fmt.Errorf("operation: %w", err)` to wrap errors
- Define sentinel errors with `errors.New` for common cases
- Never swallow errors silently (`if err != nil { ... }`)
- Error messages should be human-readable and start with lowercase

## Idiomatic Patterns

### If Error
```go
if err != nil {
    return err
}
```

### Defer Cleanup
```go
defer func() {
    if r := recover(); r != nil {
        // handle panic
    }
}()
defer db.Close()
```

### Error Wrapping
```go
return fmt.Errorf("failed to save user: %w", err)
```

### Empty Interfaces
- Use `error` interface for error returns
- Avoid `interface{}` unless truly necessary

## Comments

- Godoc-style comments for exported functions/types
- Use `//` for single-line, `/* */` for multi-line
- Comment intent, not implementation