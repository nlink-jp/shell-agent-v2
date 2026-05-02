// Package frontendlint hosts security guard rails for the frontend
// that we want enforced by `go test ./...` (no separate ESLint
// pipeline, since adding one for a single rule is overkill).
//
// The package has no runtime use; it exists solely for tests.
package frontendlint
