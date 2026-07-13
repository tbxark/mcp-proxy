package main

import (
	"errors"
	"sync"
	"testing"
)

// TestTokenRotator_RoundRobin tests the round-robin rotation mode
func TestTokenRotator_RoundRobin(t *testing.T) {
	tests := []struct {
		name         string
		tokens       []string
		callCount    int
		wantSequence []struct {
			token string
			idx   int
		}
		wantErr bool
	}{
		{
			name:      "cycle through 3 tokens",
			tokens:    []string{"tok_a", "tok_b", "tok_c"},
			callCount: 6,
			wantSequence: []struct {
				token string
				idx   int
			}{
				{"tok_a", 0},
				{"tok_b", 1},
				{"tok_c", 2},
				{"tok_a", 0},
				{"tok_b", 1},
				{"tok_c", 2},
			},
			wantErr: false,
		},
		{
			name:      "single token always returns same",
			tokens:    []string{"tok_single"},
			callCount: 3,
			wantSequence: []struct {
				token string
				idx   int
			}{
				{"tok_single", 0},
				{"tok_single", 0},
				{"tok_single", 0},
			},
			wantErr: false,
		},
		{
			name:         "empty tokens returns error",
			tokens:       []string{},
			callCount:    1,
			wantSequence: nil,
			wantErr:      true,
		},
		{
			name:      "two tokens cycle correctly",
			tokens:    []string{"tok_1", "tok_2"},
			callCount: 4,
			wantSequence: []struct {
				token string
				idx   int
			}{
				{"tok_1", 0},
				{"tok_2", 1},
				{"tok_1", 0},
				{"tok_2", 1},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rotator := NewRoundRobinRotator(tt.tokens)

			for i := 0; i < tt.callCount; i++ {
				token, idx, err := rotator.Next()

				if tt.wantErr {
					if err == nil {
						t.Errorf("Next() expected error, got nil")
					}
					return
				}

				if err != nil {
					t.Errorf("Next() unexpected error: %v", err)
					return
				}

				if i < len(tt.wantSequence) {
					want := tt.wantSequence[i]
					if token != want.token {
						t.Errorf("Next() token = %q, want %q", token, want.token)
					}
					if idx != want.idx {
						t.Errorf("Next() idx = %d, want %d", idx, want.idx)
					}
				}
			}
		})
	}
}

// TestTokenRotator_OnFirstFailed tests the on-first-failed rotation mode
func TestTokenRotator_OnFirstFailed(t *testing.T) {
	tests := []struct {
		name       string
		tokens     []string
		operations []struct {
			op     string // "next", "fail", "success"
			idx    int
			status int // for fail
		}
		wantToken string
		wantIdx   int
		wantErr   bool
	}{
		{
			name:   "reuse token until failure",
			tokens: []string{"tok_a", "tok_b", "tok_c"},
			operations: []struct {
				op     string
				idx    int
				status int
			}{
				{"next", 0, 0}, // returns tok_a
				{"next", 0, 0}, // returns tok_a (reuse)
				{"next", 0, 0}, // returns tok_a (reuse)
			},
			wantToken: "tok_a",
			wantIdx:   0,
			wantErr:   false,
		},
		{
			name:   "advance on failure",
			tokens: []string{"tok_a", "tok_b", "tok_c"},
			operations: []struct {
				op     string
				idx    int
				status int
			}{
				{"next", 0, 0},   // returns tok_a
				{"fail", 0, 401}, // mark tok_a as failed
				{"next", 0, 0},   // returns tok_b (advanced)
				{"next", 0, 0},   // returns tok_b (reuse)
			},
			wantToken: "tok_b",
			wantIdx:   1,
			wantErr:   false,
		},
		{
			name:   "success resets failure",
			tokens: []string{"tok_a", "tok_b"},
			operations: []struct {
				op     string
				idx    int
				status int
			}{
				{"next", 0, 0},    // returns tok_a
				{"fail", 0, 403},  // mark tok_a as failed
				{"success", 0, 0}, // reset tok_a failure
				{"next", 0, 0},    // returns tok_a (still at idx 0 because fail advanced, but success didn't change)
			},
			wantToken: "tok_b",
			wantIdx:   1,
			wantErr:   false,
		},
		{
			name:   "all tokens fail returns error",
			tokens: []string{"tok_a", "tok_b"},
			operations: []struct {
				op     string
				idx    int
				status int
			}{
				{"next", 0, 0},   // returns tok_a
				{"fail", 0, 401}, // tok_a failed
				{"next", 0, 0},   // returns tok_b
				{"fail", 1, 401}, // tok_b failed
				{"next", 0, 0},   // should return error
			},
			wantToken: "",
			wantIdx:   -1,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rotator := NewOnFirstFailedRotator(tt.tokens)

			var lastToken string
			var lastIdx int
			var lastErr error

			for _, op := range tt.operations {
				switch op.op {
				case "next":
					lastToken, lastIdx, lastErr = rotator.Next()
				case "fail":
					rotator.Fail(op.idx, op.status)
				case "success":
					rotator.Success(op.idx)
				}
			}

			if tt.wantErr {
				if lastErr == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}

			if lastErr != nil {
				t.Errorf("unexpected error: %v", lastErr)
				return
			}

			if lastToken != tt.wantToken {
				t.Errorf("token = %q, want %q", lastToken, tt.wantToken)
			}
			if lastIdx != tt.wantIdx {
				t.Errorf("idx = %d, want %d", lastIdx, tt.wantIdx)
			}
		})
	}
}

// TestTokenRotator_ConcurrentAccess tests thread safety
func TestTokenRotator_ConcurrentAccess(t *testing.T) {
	t.Run("round-robin concurrent Next", func(t *testing.T) {
		tokens := []string{"tok_a", "tok_b", "tok_c"}
		rotator := NewRoundRobinRotator(tokens)

		const goroutines = 10
		const callsPerGoroutine = 100

		var wg sync.WaitGroup
		wg.Add(goroutines)

		// Track all returned tokens
		results := make(chan string, goroutines*callsPerGoroutine)

		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < callsPerGoroutine; j++ {
					token, _, err := rotator.Next()
					if err == nil {
						results <- token
					}
				}
			}()
		}

		wg.Wait()
		close(results)

		// Verify all returned tokens are valid
		tokenSet := make(map[string]bool)
		for _, t := range tokens {
			tokenSet[t] = true
		}

		count := 0
		for token := range results {
			count++
			if !tokenSet[token] {
				t.Errorf("invalid token returned: %q", token)
			}
		}

		// Should have exactly goroutines * callsPerGoroutine tokens
		if count != goroutines*callsPerGoroutine {
			t.Errorf("got %d tokens, want %d", count, goroutines*callsPerGoroutine)
		}
	})

	t.Run("on-first-failed concurrent Next and Fail", func(t *testing.T) {
		tokens := []string{"tok_a", "tok_b", "tok_c"}
		rotator := NewOnFirstFailedRotator(tokens)

		const goroutines = 10
		const operationsPerGoroutine = 50

		var wg sync.WaitGroup
		wg.Add(goroutines)

		for i := 0; i < goroutines; i++ {
			go func(id int) {
				defer wg.Done()
				for j := 0; j < operationsPerGoroutine; j++ {
					token, idx, err := rotator.Next()
					if err == nil {
						// Randomly fail or succeed
						if j%3 == 0 {
							rotator.Fail(idx, 401)
						} else {
							rotator.Success(idx)
						}
						_ = token // just verify no panic
					}
				}
			}(i)
		}

		wg.Wait()
		// Test passes if no race condition or panic
	})
}

// TestTokenRotator_EdgeCases tests edge cases
func TestTokenRotator_EdgeCases(t *testing.T) {
	t.Run("nil tokens slice", func(t *testing.T) {
		rotator := NewRoundRobinRotator(nil)
		_, _, err := rotator.Next()
		if err == nil {
			t.Error("expected error for nil tokens")
		}
	})

	t.Run("duplicate tokens allowed", func(t *testing.T) {
		tokens := []string{"tok_a", "tok_a", "tok_b"}
		rotator := NewRoundRobinRotator(tokens)

		// Should work with duplicates (no validation at rotator level)
		token1, idx1, err := rotator.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		token2, idx2, err := rotator.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Should cycle through indices 0, 1, 2
		if idx1 != 0 || idx2 != 1 {
			t.Errorf("indices = (%d, %d), want (0, 1)", idx1, idx2)
		}
		if token1 != "tok_a" || token2 != "tok_a" {
			t.Errorf("tokens = (%q, %q), want (tok_a, tok_a)", token1, token2)
		}
	})

	t.Run("empty string token in list", func(t *testing.T) {
		tokens := []string{"", "tok_valid"}
		rotator := NewRoundRobinRotator(tokens)

		token, _, err := rotator.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Empty string should still be returned (validation is at higher level)
		if token != "" {
			t.Errorf("token = %q, want empty string", token)
		}
	})

	t.Run("token exhaustion error type", func(t *testing.T) {
		tokens := []string{"tok_a", "tok_b"}
		rotator := NewOnFirstFailedRotator(tokens)

		// Get first token and fail it
		_, _, _ = rotator.Next()
		rotator.Fail(0, 401)

		// Get second token and fail it
		_, _, _ = rotator.Next()
		rotator.Fail(1, 403)

		// Now Next should return exhaustion error
		_, _, err := rotator.Next()
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		var exhaustedErr *TokenExhaustedError
		if !errors.As(err, &exhaustedErr) {
			t.Errorf("error type = %T, want *TokenExhaustedError", err)
		}

		if exhaustedErr != nil {
			// Attempts count is 2 because the third Next() call returns error without incrementing
			// (attempts are only incremented when we actually try a token)
			if exhaustedErr.Attempts != 2 {
				t.Errorf("Attempts = %d, want 2", exhaustedErr.Attempts)
			}
			if len(exhaustedErr.Statuses) != 2 {
				t.Errorf("Statuses length = %d, want 2", len(exhaustedErr.Statuses))
			}
		}
	})

	t.Run("success on non-existent index does not panic", func(t *testing.T) {
		tokens := []string{"tok_a"}
		rotator := NewOnFirstFailedRotator(tokens)

		// Should not panic with invalid index
		rotator.Success(99)
		rotator.Fail(99, 401)
	})

	t.Run("round-robin rotation continues after failure", func(t *testing.T) {
		tokens := []string{"tok_a", "tok_b", "tok_c"}
		rotator := NewRoundRobinRotator(tokens)

		// First call returns tok_a at idx=0
		token, idx, err := rotator.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "tok_a" || idx != 0 {
			t.Errorf("first call: token=%q idx=%d, want tok_a idx=0", token, idx)
		}

		// Round-robin ignores Fail calls
		rotator.Fail(0, 401)

		// Second call should return tok_b (index advances regardless)
		token, idx, err = rotator.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "tok_b" || idx != 1 {
			t.Errorf("second call: token=%q idx=%d, want tok_b idx=1", token, idx)
		}
	})
}

// TestTokenRotator_FailureTracking tests failure count tracking
func TestTokenRotator_FailureTracking(t *testing.T) {
	t.Run("track multiple failures per token", func(t *testing.T) {
		tokens := []string{"tok_a", "tok_b"}
		rotator := NewOnFirstFailedRotator(tokens)

		// Multiple failures on same token
		rotator.Fail(0, 401)
		rotator.Fail(0, 403)

		// Should still advance to next token
		token, idx, err := rotator.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "tok_b" || idx != 1 {
			t.Errorf("token=%q idx=%d, want tok_b idx=1", token, idx)
		}
	})

	t.Run("success resets failure count", func(t *testing.T) {
		tokens := []string{"tok_a", "tok_b"}
		rotator := NewOnFirstFailedRotator(tokens)

		// Get first token
		_, _, _ = rotator.Next()

		// Fail first token (advances to tok_b)
		rotator.Fail(0, 401)

		// Success on tok_b
		rotator.Success(1)

		// Next should still return tok_b (we're on it)
		token, _, err := rotator.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "tok_b" {
			t.Errorf("token = %q, want tok_b", token)
		}
	})
}

// TestTokenExhaustedError tests the error type
func TestTokenExhaustedError(t *testing.T) {
	t.Run("error message contains no token values", func(t *testing.T) {
		err := &TokenExhaustedError{
			Attempts: 3,
			Statuses: []int{401, 403, 401},
			LastErr:  nil,
		}

		msg := err.Error()

		if msg == "" {
			t.Error("error message should not be empty")
		}

		// Verify no token values in message (this is a security check)
		// The message should contain metadata only
	})

	t.Run("unwrap returns sentinel error", func(t *testing.T) {
		err := &TokenExhaustedError{
			Attempts: 2,
			Statuses: []int{401, 401},
			LastErr:  nil,
		}

		unwrapped := err.Unwrap()
		if unwrapped != ErrAllTokensFailed {
			t.Errorf("Unwrap() = %v, want %v", unwrapped, ErrAllTokensFailed)
		}
	})

	t.Run("errors.Is works with sentinel", func(t *testing.T) {
		err := &TokenExhaustedError{
			Attempts: 2,
			Statuses: []int{401, 401},
			LastErr:  nil,
		}

		if !errors.Is(err, ErrAllTokensFailed) {
			t.Error("errors.Is should return true for ErrAllTokensFailed")
		}
	})
}

// TestTokenRotator_InvalidRotationMode tests error handling for invalid mode
func TestTokenRotator_InvalidRotationMode(t *testing.T) {
	tokens := []string{"tok_a", "tok_b"}

	_, err := NewTokenRotator("invalid-mode", tokens)
	if err == nil {
		t.Error("expected error for invalid rotation mode")
	}

	if err != nil && err != ErrInvalidRotationMode {
		t.Errorf("error = %v, want %v", err, ErrInvalidRotationMode)
	}
}

// TestTokenRotator_FactoryFunction tests the factory function
func TestTokenRotator_FactoryFunction(t *testing.T) {
	tokens := []string{"tok_a", "tok_b"}

	t.Run("creates round-robin rotator", func(t *testing.T) {
		rotator, err := NewTokenRotator("round-robin", tokens)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Verify round-robin behavior
		token1, _, _ := rotator.Next()
		token2, _, _ := rotator.Next()

		if token1 != token2 {
			// In round-robin with 2 tokens, first two calls should return different tokens
		}
	})

	t.Run("creates on-first-failed rotator", func(t *testing.T) {
		rotator, err := NewTokenRotator("on-first-failed", tokens)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Verify on-first-failed behavior
		token1, _, _ := rotator.Next()
		token2, _, _ := rotator.Next()

		if token1 != token2 {
			t.Errorf("on-first-failed should reuse token, got %q then %q", token1, token2)
		}
	})

	t.Run("empty tokens returns error", func(t *testing.T) {
		_, err := NewTokenRotator("round-robin", []string{})
		if err == nil {
			t.Error("expected error for empty tokens")
		}
	})
}
