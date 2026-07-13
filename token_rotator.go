package main

import (
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors for token rotation
var (
	ErrNoTokens            = errors.New("no authentication tokens configured")
	ErrAllTokensFailed     = errors.New("all authentication tokens failed")
	ErrInvalidRotationMode = errors.New("invalid rotation mode")
)

// TokenExhaustedError is returned when all tokens have failed authentication
type TokenExhaustedError struct {
	Attempts int   // Total number of authentication attempts
	Statuses []int // HTTP status codes received (401, 403)
	LastErr  error // Last error received (may be nil)
}

// Error returns a formatted error message without token values
func (e *TokenExhaustedError) Error() string {
	return fmt.Sprintf("all authentication tokens failed after %d attempts (statuses: %v)", e.Attempts, e.Statuses)
}

// Unwrap returns the sentinel error for errors.Is compatibility
func (e *TokenExhaustedError) Unwrap() error {
	return ErrAllTokensFailed
}

// TokenRotator manages token rotation state and provides thread-safe token selection
type TokenRotator interface {
	// Next returns the next token to use and its index
	Next() (token string, idx int, err error)
	// Fail records an authentication failure for the token at the given index
	Fail(idx int, status int)
	// Success records a successful authentication, resetting failure count
	Success(idx int)
}

// rotatorState holds shared state for token rotation implementations
type rotatorState struct {
	tokens []string
	mu     sync.Mutex
	idx    int   // Current token index (0 to len(tokens)-1)
	fails  []int // Failure count per token
}

// roundRobinRotator cycles through tokens sequentially per request
type roundRobinRotator struct {
	*rotatorState
}

// NewRoundRobinRotator creates a new round-robin token rotator
func NewRoundRobinRotator(tokens []string) TokenRotator {
	if len(tokens) == 0 {
		return &roundRobinRotator{rotatorState: &rotatorState{tokens: tokens}}
	}
	return &roundRobinRotator{
		rotatorState: &rotatorState{
			tokens: tokens,
			idx:    0,
			fails:  make([]int, len(tokens)),
		},
	}
}

// Next returns the next token in round-robin order and advances the index
func (r *roundRobinRotator) Next() (string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.tokens) == 0 {
		return "", -1, ErrNoTokens
	}

	token := r.tokens[r.idx]
	idx := r.idx
	r.idx = (r.idx + 1) % len(r.tokens)
	return token, idx, nil
}

// Fail is a no-op for round-robin (rotation happens on every Next call)
func (r *roundRobinRotator) Fail(idx int, status int) {
	// Round-robin ignores failures - rotation happens on every Next() call
}

// Success is a no-op for round-robin
func (r *roundRobinRotator) Success(idx int) {
	// Round-robin ignores success - rotation happens on every Next() call
}

// onFirstFailedRotator reuses the current token until failure, then advances
type onFirstFailedRotator struct {
	*rotatorState
	attempts int   // Total authentication attempts
	statuses []int // HTTP status codes from failures
}

// NewOnFirstFailedRotator creates a new on-first-failed token rotator
func NewOnFirstFailedRotator(tokens []string) TokenRotator {
	if len(tokens) == 0 {
		return &onFirstFailedRotator{rotatorState: &rotatorState{tokens: tokens}}
	}
	return &onFirstFailedRotator{
		rotatorState: &rotatorState{
			tokens: tokens,
			idx:    0,
			fails:  make([]int, len(tokens)),
		},
		attempts: 0,
		statuses: make([]int, 0),
	}
}

// Next returns the current token (does not advance unless all tokens have failed)
func (r *onFirstFailedRotator) Next() (string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.tokens) == 0 {
		return "", -1, ErrNoTokens
	}

	// Check if all tokens have failed
	allFailed := true
	for i := 0; i < len(r.tokens); i++ {
		if r.fails[i] == 0 {
			allFailed = false
			break
		}
	}

	if allFailed {
		return "", -1, &TokenExhaustedError{
			Attempts: r.attempts,
			Statuses: r.statuses,
			LastErr:  nil,
		}
	}

	// Find the next non-failed token starting from current idx
	startIdx := r.idx
	for {
		if r.fails[r.idx] == 0 {
			token := r.tokens[r.idx]
			idx := r.idx
			r.attempts++
			return token, idx, nil
		}
		r.idx = (r.idx + 1) % len(r.tokens)
		if r.idx == startIdx {
			// All tokens have failed (shouldn't happen if allFailed check works)
			return "", -1, &TokenExhaustedError{
				Attempts: r.attempts,
				Statuses: r.statuses,
				LastErr:  nil,
			}
		}
	}
}

// Fail marks the token at idx as failed and advances to next token
func (r *onFirstFailedRotator) Fail(idx int, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if idx >= 0 && idx < len(r.fails) {
		r.fails[idx]++
		r.statuses = append(r.statuses, status)
		// Advance to next token for next call
		r.idx = (r.idx + 1) % len(r.tokens)
	}
}

// Success resets the failure count for the token at idx
func (r *onFirstFailedRotator) Success(idx int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if idx >= 0 && idx < len(r.fails) {
		r.fails[idx] = 0
	}
}

// NewTokenRotator creates a new token rotator based on the specified mode
func NewTokenRotator(mode string, tokens []string) (TokenRotator, error) {
	if len(tokens) == 0 {
		return nil, ErrNoTokens
	}

	switch mode {
	case "round-robin":
		return NewRoundRobinRotator(tokens), nil
	case "on-first-failed":
		return NewOnFirstFailedRotator(tokens), nil
	default:
		return nil, ErrInvalidRotationMode
	}
}
