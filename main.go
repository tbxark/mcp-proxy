package main

import (
	"flag"
	"fmt"
	"log"
)

var BuildVersion = "dev"

func main() {
	conf := flag.String("config", "config.json", "path to config file or a http(s) url")
	insecure := flag.Bool("insecure", false, "allow insecure HTTPS connections by skipping TLS certificate verification")
	expandEnv := flag.Bool("expand-env", true, "expand environment variables in config file")
	httpHeaders := flag.String("http-headers", "", "optional HTTP headers for config URL, format: 'Key1:Value1;Key2:Value2'")
	httpTimeout := flag.Int("http-timeout", 10, "HTTP timeout in seconds when fetching config from URL")
	checkConfig := flag.Bool("check-config", false, "load and validate the config, then exit without starting the server")

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
	config, err := load(*conf, *insecure, *expandEnv, *httpHeaders, *httpTimeout)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if *checkConfig {
		fmt.Printf("Config OK: %d MCP server(s) configured\n", len(config.McpServers))
		return
	}
	err = startHTTPServer(config)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
