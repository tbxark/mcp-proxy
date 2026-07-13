package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStartHTTPServer_InvalidBaseURL(t *testing.T) {
	config := &Config{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "://invalid-url",
			Addr:    ":9090",
			Name:    "test",
			Version: "1.0",
		},
		McpServers: map[string]*MCPClientConfigV2{},
	}

	err := startHTTPServer(config)
	if err == nil {
		t.Error("startHTTPServer() expected error for invalid URL, got nil")
	}
}

func TestStartHTTPServer_NewMCPClientError(t *testing.T) {
	config := &Config{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test",
			Version: "1.0",
		},
		McpServers: map[string]*MCPClientConfigV2{
			"invalid": {
				TransportType: "invalid-type",
				Options:       &OptionsV2{},
			},
		},
	}

	err := startHTTPServer(config)
	if err == nil {
		t.Error("startHTTPServer() expected error for invalid client config, got nil")
	}
}

func TestStartHTTPServer_NewMCPServerError(t *testing.T) {
	config := &Config{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test",
			Version: "1.0",
			Type:    "invalid-type",
		},
		McpServers: map[string]*MCPClientConfigV2{
			"test": {
				Command: "echo",
				Options: &OptionsV2{},
			},
		},
	}

	err := startHTTPServer(config)
	if err == nil {
		t.Error("startHTTPServer() expected error for invalid server type, got nil")
	}
}

func TestStartHTTPServer_DisabledClient(t *testing.T) {
	config := &Config{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":0",
			Name:    "test",
			Version: "1.0",
			Type:    MCPServerTypeSSE,
		},
		McpServers: map[string]*MCPClientConfigV2{
			"disabled": {
				Command: "echo",
				Options: &OptionsV2{
					Disabled: true,
				},
			},
		},
	}

	serverStarted := make(chan bool, 1)
	go func() {
		err := startHTTPServer(config)
		if err != nil {
			serverStarted <- false
		}
	}()

	select {
	case <-serverStarted:
	case <-time.After(100 * time.Millisecond):
	}
}

func TestChainMiddleware(t *testing.T) {
	callOrder := []string{}

	middleware1 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callOrder = append(callOrder, "m1-before")
			next.ServeHTTP(w, r)
			callOrder = append(callOrder, "m1-after")
		})
	}

	middleware2 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callOrder = append(callOrder, "m2-before")
			next.ServeHTTP(w, r)
			callOrder = append(callOrder, "m2-after")
		})
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callOrder = append(callOrder, "handler")
		w.WriteHeader(http.StatusOK)
	})

	chained := chainMiddleware(handler, middleware1, middleware2)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)

	expected := []string{"m2-before", "m1-before", "handler", "m1-after", "m2-after"}
	if len(callOrder) != len(expected) {
		t.Errorf("call order length = %d, want %d", len(callOrder), len(expected))
	}

	for i, v := range expected {
		if i >= len(callOrder) || callOrder[i] != v {
			t.Errorf("callOrder[%d] = %v, want %v", i, callOrder[i], v)
		}
	}
}

func TestChainMiddleware_Empty(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	chained := chainMiddleware(handler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNewAuthMiddleware_NoTokens(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := newAuthMiddleware([]string{})
	chained := middleware(handler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d (should allow when no tokens configured)", rec.Code, http.StatusOK)
	}
}

func TestNewAuthMiddleware_ValidToken(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tokens := []string{"valid-token-1", "valid-token-2"}
	middleware := newAuthMiddleware(tokens)
	chained := middleware(handler)

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{
			name:       "valid token 1",
			authHeader: "Bearer valid-token-1",
			wantStatus: http.StatusOK,
		},
		{
			name:       "valid token 2",
			authHeader: "Bearer valid-token-2",
			wantStatus: http.StatusOK,
		},
		{
			name:       "invalid token",
			authHeader: "Bearer invalid-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "no auth header",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "token without Bearer prefix but still valid",
			authHeader: "valid-token-1",
			wantStatus: http.StatusOK,
		},
		{
			name:       "empty bearer",
			authHeader: "Bearer ",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			chained.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("Status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestNewAuthMiddleware_TokenTrimming(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tokens := []string{"valid-token"}
	middleware := newAuthMiddleware(tokens)
	chained := middleware(handler)

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{
			name:       "token with trailing spaces is trimmed",
			authHeader: "Bearer valid-token   ",
			wantStatus: http.StatusOK,
		},
		{
			name:       "exact token match",
			authHeader: "Bearer valid-token",
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Authorization", tt.authHeader)
			rec := httptest.NewRecorder()
			chained.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("Status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestLoggerMiddleware(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := loggerMiddleware("test-prefix")
	chained := middleware(handler)

	req := httptest.NewRequest("POST", "/test/path", nil)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRecoverMiddleware_NoPanic(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := recoverMiddleware("test")
	chained := middleware(handler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRecoverMiddleware_WithPanic(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	middleware := recoverMiddleware("test")
	chained := middleware(handler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	// Should not panic, should recover
	chained.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestRecoverMiddleware_PanicWithDifferentTypes(t *testing.T) {
	tests := []struct {
		name        string
		panicValue  any
		wantStatus  int
		wantNoPanic bool
	}{
		{
			name:        "panic with string",
			panicValue:  "error string",
			wantStatus:  http.StatusInternalServerError,
			wantNoPanic: true,
		},
		{
			name:        "panic with error",
			panicValue:  http.ErrHandlerTimeout,
			wantStatus:  http.StatusInternalServerError,
			wantNoPanic: true,
		},
		{
			name:        "panic with nil",
			panicValue:  nil,
			wantStatus:  http.StatusInternalServerError,
			wantNoPanic: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				panic(tt.panicValue)
			})

			middleware := recoverMiddleware("test")
			chained := middleware(handler)

			req := httptest.NewRequest("GET", "/", nil)
			rec := httptest.NewRecorder()

			// Should recover and not propagate panic
			defer func() {
				if r := recover(); r != nil && tt.wantNoPanic {
					t.Error("panic should have been recovered")
				}
			}()

			chained.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("Status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestMiddlewareFunc_Type(t *testing.T) {
	var middlewares []MiddlewareFunc

	middlewares = append(middlewares, newAuthMiddleware([]string{"token"}))
	middlewares = append(middlewares, loggerMiddleware("test"))
	middlewares = append(middlewares, recoverMiddleware("test"))

	if len(middlewares) != 3 {
		t.Errorf("middleware count = %d, want 3", len(middlewares))
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	chained := chainMiddleware(handler, middlewares...)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestAuthMiddleware_EdgeCases(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("case insensitive bearer", func(t *testing.T) {
		middleware := newAuthMiddleware([]string{"test-token"})
		chained := middleware(handler)

		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "bearer test-token")
		rec := httptest.NewRecorder()
		chained.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Error("Expected unauthorized for lowercase 'bearer', got OK")
		}
	})

	t.Run("bearer with no space", func(t *testing.T) {
		middleware := newAuthMiddleware([]string{"test-token"})
		chained := middleware(handler)

		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearertest-token")
		rec := httptest.NewRecorder()
		chained.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Error("Expected unauthorized for 'Bearer' without space, got OK")
		}
	})
}

func TestRecoverMiddleware_ContextCancel(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	middleware := recoverMiddleware("test")
	chained := middleware(handler)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)
}

func TestMiddleware_Composition(t *testing.T) {
	callOrder := strings.Builder{}

	middleware1 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callOrder.WriteString("1")
			next.ServeHTTP(w, r)
		})
	}

	middleware2 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callOrder.WriteString("2")
			next.ServeHTTP(w, r)
		})
	}

	middleware3 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callOrder.WriteString("3")
			next.ServeHTTP(w, r)
		})
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callOrder.WriteString("H")
		w.WriteHeader(http.StatusOK)
	})

	chained := chainMiddleware(handler, middleware1, middleware2, middleware3)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)

	order := callOrder.String()
	if order != "321H" {
		t.Errorf("Call order = %s, want 321H", order)
	}
}
