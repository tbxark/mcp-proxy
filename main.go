package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

var BuildVersion = "dev"

func main() {
	conf := flag.String("config", "config.json", "path to config file or a http(s) url")
	insecure := flag.Bool("insecure", false, "allow insecure HTTPS connections by skipping TLS certificate verification")
	expandEnv := flag.Bool("expand-env", true, "expand environment variables in config file")
	httpHeaders := flag.String("http-headers", "", "optional HTTP headers for config URL, format: 'Key1:Value1;Key2:Value2'")
	httpTimeout := flag.Int("http-timeout", 10, "HTTP timeout in seconds when fetching config from URL")
	authorize := flag.String("authorize", "", "run a one-time interactive OAuth authorization for the named mcpServers entry, then exit. Opens a browser; run this by hand, not from the daemon/service.")
	checkConfig := flag.Bool("check-config", false, "load and validate the config, then exit without starting the server")
	var logLevel slog.Level
	flag.TextVar(&logLevel, "log-level", slog.LevelInfo, "log level (debug, info, warn, error)")

	version := flag.Bool("version", false, "print version and exit")
	help := flag.Bool("help", false, "print help and exit")
	flag.Parse()
	if *help {
		flag.Usage()
		return
	}
	if *version {
		fmt.Println(BuildVersion)
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))
	if *authorize != "" {
		if err := runAuthorize(*conf, *authorize, *insecure, *expandEnv, *httpHeaders, *httpTimeout); err != nil {
			slog.Error("Failed to authorize server", "server", *authorize, "err", err)
			os.Exit(1)
		}
		return
	}
	config, err := load(*conf, *insecure, *expandEnv, *httpHeaders, *httpTimeout)
	if err != nil {
		slog.Error("Failed to load config", "err", err)
		os.Exit(1)
	}
	if *checkConfig {
		fmt.Printf("Config OK: %d MCP server(s) configured\n", len(config.McpServers))
		return
	}
	err = startHTTPServer(config)
	if err != nil {
		slog.Error("Failed to start server", "err", err)
		os.Exit(1)
	}
}
