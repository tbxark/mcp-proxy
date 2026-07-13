job "mcp-proxy" {
  datacenters = ["dc1"]
  region      = "global"
  type        = "service"

  constraint {
    attribute = "${node.unique.name}"
    value     = "cfly-swa"
  }

  meta {
    project     = "mcp-proxy"
    repo        = "bhd/mcp-proxy"
    component   = "proxy"
    service_cwd = "/home/bhd/Documents/Projects/bhd/mcp-proxy"
  }

  group "mcp-proxy" {
    count = 1

    meta = {
      project     = "mcp-proxy"
      repo        = "bhd/mcp-proxy"
      component   = "proxy"
      service_cwd = "/home/bhd/Documents/Projects/bhd/mcp-proxy"
    }

    network {
      mode = "host"

      port "http" {
        static       = 9090
        to           = 9090
        host_network = "tailscale"
      }
    }

    task "mcp-proxy" {
      driver = "docker"
      user   = "root"

      template {
        destination = "${NOMAD_TASK_DIR}/config.json"
        change_mode = "restart"
        data        = <<-EOH
{
  "mcpProxy": {
    "baseURL": "http://mcp-proxy.tail05fddd.ts.net:9090",
    "addr": ":9090",
    "name": "MCP Proxy",
    "version": "1.0.0",
    "type": "streamable-http",
    "options": {
      "panicIfInvalid": false,
      "logEnabled": true,
      "authTokens": []
    }
  },
  "mcpServers": {
    "websearch": {
      "url": "https://mcp.exa.ai/mcp?tools=web_search_exa",
      "options": {
        "authTokens": []
      }
    },
    "context7": {
      "url": "https://mcp.context7.com/mcp",
      "options": {
        "authTokens": []
      }
    },
    "grep_app": {
      "url": "https://mcp.grep.app",
      "options": {
        "authTokens": []
      }
    }
  }
}
EOH
      }

      config {
        image        = "ghcr.io/tbxark/mcp-proxy:latest"
        network_mode = "host"
        ports        = ["http"]
        volumes      = ["local/config.json:/config/config.json:ro"]
        logging {
          type = "json-file"
          config {
            max-size = "25m"
            max-file = "7"
          }
        }
      }

      logs {
        max_files     = 7
        max_file_size = 25
      }

      resources {
        cpu    = 500
        memory = 1024
      }

      service {
        name = "mcp-proxy"
        port = "http"

        tags = [
          "project:mcp-proxy",
          "component:proxy",
          "repo:bhd/mcp-proxy",
          "cwd:/home/bhd/Documents/Projects/bhd/mcp-proxy",
          "plane:${node.unique.name}",
        ]

        check {
          name     = "tcp-mcp-proxy"
          type     = "tcp"
          interval = "10s"
          timeout  = "3s"
        }
      }

      restart {
        attempts = 5
        interval = "10m"
        delay    = "10s"
        mode     = "delay"
      }
    }
  }

  update {
    max_parallel     = 1
    min_healthy_time = "15s"
    healthy_deadline = "5m"
    auto_revert      = true
    health_check     = "checks"
  }

  reschedule {
    attempts       = 1
    interval       = "1m"
    delay          = "30s"
    delay_function = "exponential"
    max_delay      = "10m"
    unlimited      = false
  }
}
