# Quickstart: Token Rotation and Proxy Support

**Feature**: 001-token-rotation-proxy  
**Date**: 2026-02-19

## Overview

This guide demonstrates how to configure and use authentication token rotation in MCP Proxy. Proxy support is optional and can be added independently.

**Core Feature**: Token rotation (round-robin and on-first-failed modes)  
**Optional Feature**: Proxy routing (HTTP/HTTPS/SOCKS5)

## Prerequisites

- MCP Proxy installed and configured
- Multiple authentication tokens (for rotation testing)
- Optional: HTTP/SOCKS5 proxy server (only if using proxy feature)

## Basic Token Rotation

### Round-Robin Mode

Distributes load across multiple tokens by cycling through them sequentially.

**config.json**:
```json
{
  "mcpServers": {
    "my-server": {
      "transportType": "sse",
      "url": "https://mcp.example.com/sse",
      "options": {
        "auth": {
          "tokens": ["tok_a", "tok_b", "tok_c"],
          "rotationMode": "round-robin"
        }
      }
    }
  }
}
```

**Behavior**:
- Request 1 uses `tok_a`
- Request 2 uses `tok_b`
- Request 3 uses `tok_c`
- Request 4 uses `tok_a` (cycle repeats)

**Use Case**: Load distribution, rate limit avoidance

---

### On-First-Failed Mode

Automatically fails over to backup tokens when authentication fails.

**config.json**:
```json
{
  "mcpServers": {
    "my-server": {
      "transportType": "streamable-http",
      "url": "https://mcp.example.com/api",
      "options": {
        "auth": {
          "tokens": ["tok_primary", "tok_backup1", "tok_backup2"],
          "rotationMode": "on-first-failed",
          "maxRetries": 3
        }
      }
    }
  }
}
```

**Behavior**:
- All requests use `tok_primary` until it fails (401/403)
- On failure, automatically retries with `tok_backup1`
- If `tok_backup1` fails, retries with `tok_backup2`
- If all tokens fail, returns error

**Use Case**: High availability, automatic failover

---

## Proxy Configuration

### HTTP Proxy

Route connections through HTTP proxy server.

**config.json**:
```json
{
  "mcpServers": {
    "my-server": {
      "transportType": "sse",
      "url": "https://mcp.example.com/sse",
      "options": {
        "proxy": {
          "url": "http://proxy.internal:8080"
        }
      }
    }
  }
}
```

**With Authentication**:
```json
{
  "mcpServers": {
    "my-server": {
      "transportType": "sse",
      "url": "https://mcp.example.com/sse",
      "options": {
        "proxy": {
          "url": "http://proxy.internal:8080",
          "auth": {
            "username": "proxyuser",
            "password": "proxypass"
          }
        }
      }
    }
  }
}
```

---

### SOCKS5 Proxy

Route connections through SOCKS5 proxy server.

**config.json**:
```json
{
  "mcpServers": {
    "my-server": {
      "transportType": "streamable-http",
      "url": "https://mcp.example.com/api",
      "options": {
        "proxy": {
          "url": "socks5://127.0.0.1:1080",
          "auth": {
            "username": "socksuser",
            "password": "sockspass"
          }
        }
      }
    }
  }
}
```

---

### Environment Variable Fallback

Use system proxy environment variables when no proxy config is specified.

**Environment**:
```bash
export HTTP_PROXY=http://proxy.internal:8080
export HTTPS_PROXY=https://proxy.internal:8443
```

**config.json**:
```json
{
  "mcpServers": {
    "my-server": {
      "transportType": "sse",
      "url": "https://mcp.example.com/sse",
      "options": {
        "proxy": {
          "useEnv": true
        }
      }
    }
  }
}
```

**Note**: If `proxy.url` is not specified, environment variables are used automatically.

---

## Combined Configuration

### Token Rotation + Proxy

Use both features together for maximum reliability and compliance.

**config.json**:
```json
{
  "mcpServers": {
    "my-server": {
      "transportType": "streamable-http",
      "url": "https://mcp.example.com/api",
      "options": {
        "auth": {
          "tokens": ["tok_primary", "tok_backup"],
          "rotationMode": "on-first-failed",
          "maxRetries": 2
        },
        "proxy": {
          "url": "http://proxy.internal:8080",
          "auth": {
            "username": "proxyuser",
            "password": "proxypass"
          }
        }
      }
    }
  }
}
```

**Behavior**:
- All requests routed through proxy
- Token rotation applies to requests through proxy
- If token fails (401/403), retry with next token through same proxy
- If proxy fails (407), return error without token rotation

---

## Backward Compatibility

### Legacy AuthTokens Field

Existing configurations using `authTokens` continue to work.

**Old config (still works)**:
```json
{
  "mcpServers": {
    "my-server": {
      "transportType": "sse",
      "url": "https://mcp.example.com/sse",
      "options": {
        "authTokens": ["tok_a", "tok_b"]
      }
    }
  }
}
```

**Behavior**: Automatically mapped to `auth.tokens` with `rotationMode: "round-robin"` (with warning logged)

**Recommended**: Migrate to new `auth` config for explicit control.

---

## Testing Your Configuration

### 1. Verify Token Rotation

**Test round-robin**:
```bash
# Send 3 requests and check logs for rotation events
curl -X POST http://localhost:9090/mcp/v1/tools/list
curl -X POST http://localhost:9090/mcp/v1/tools/list
curl -X POST http://localhost:9090/mcp/v1/tools/list
```

**Expected**: Logs show rotation events (token indices, not values): `using token index 0`, `using token index 1`, `using token index 2`

---

### 2. Verify Failover

**Test on-first-failed**:
```bash
# Revoke primary token, send request
# Should automatically retry with backup token
curl -X POST http://localhost:9090/mcp/v1/tools/list
```

**Expected**: Request succeeds with backup token after primary fails

---

### 3. Verify Proxy Routing

**Test proxy**:
```bash
# Check proxy logs to verify requests are routed through proxy
curl -X POST http://localhost:9090/mcp/v1/tools/list
```

**Expected**: Proxy server logs show incoming requests from MCP Proxy

---

## Troubleshooting

### All Tokens Failed

**Error**: `all authentication tokens failed after N attempts`

**Cause**: All configured tokens returned 401/403

**Solution**:
1. Verify tokens are valid and not expired
2. Check token permissions for the MCP server
3. Review server logs for authentication errors

---

### Proxy Connection Failed

**Error**: `proxy connection failed: dial tcp: lookup proxy.internal: no such host`

**Cause**: Proxy URL is incorrect or proxy server is unreachable

**Solution**:
1. Verify proxy URL is correct
2. Check network connectivity to proxy server
3. Verify proxy server is running

---

### Proxy Authentication Required

**Error**: `proxy authentication required (407)`

**Cause**: Proxy requires authentication but credentials not provided

**Solution**:
1. Add `proxy.auth` configuration with username/password
2. Verify proxy credentials are correct

---

### Headers.Authorization Conflict

**Warning**: `headers.Authorization ignored because auth.tokens is set`

**Cause**: Both `headers.Authorization` and `auth.tokens` are configured

**Solution**:
1. Remove `headers.Authorization` from config
2. Use only `auth.tokens` for authentication
3. Warning is informational; `auth.tokens` takes precedence

---

## Performance Considerations

### Token Rotation Overhead

- **Round-robin**: <1ms per request (mutex lock + array access)
- **On-first-failed**: <1ms per request (no retry unless auth fails)
- **Retry overhead**: ~100ms per retry (depends on server response time)

### Proxy Routing Overhead

- **HTTP proxy**: 1-50ms additional latency (depends on proxy location)
- **SOCKS5 proxy**: 1-50ms additional latency + handshake overhead
- **Environment fallback**: No overhead (uses standard Go proxy mechanism)

---

## Security Best Practices

### Token Management

1. **Never commit tokens to version control**
   - Use environment variables: `"tokens": ["${TOKEN_PRIMARY}", "${TOKEN_BACKUP}"]`
   - Use secret management systems (Vault, AWS Secrets Manager)

2. **Rotate tokens regularly**
   - Set up automated token rotation
   - Use on-first-failed mode for zero-downtime rotation

3. **Monitor token usage**
   - Enable logging to track token rotation events (indices only, never values)
   - Alert on token exhaustion errors
   - Review logs for authentication failure patterns

### Proxy Credentials

1. **Use environment variables for proxy passwords**
   ```bash
   export PROXY_USER=proxyuser
   export PROXY_PASS=proxypass
   ```
   ```json
   {
     "proxy": {
       "url": "http://proxy.internal:8080",
       "auth": {
         "username": "${PROXY_USER}",
         "password": "${PROXY_PASS}"
       }
     }
   }
   ```

2. **Restrict config file permissions**
   ```bash
   chmod 600 config.json
   ```

3. **Use HTTPS/SOCKS5 for encrypted proxy connections**

---

## Next Steps

1. **Configure token rotation** for your MCP servers
2. **Test failover behavior** with revoked tokens
3. **Set up proxy routing** if required by network policy
4. **Monitor logs** for rotation and proxy events
5. **Review security practices** for token and credential management

## Related Documentation

- [Configuration Reference](../../docs/CONFIGURATION.md) - Full configuration options
- [Usage Guide](../../docs/USAGE.md) - Command-line flags and endpoints
- [Deployment Guide](../../docs/DEPLOYMENT.md) - Docker and production deployment
