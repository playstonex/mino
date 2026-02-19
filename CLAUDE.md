# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Mihomo (Meta Kernel) is a network proxy tool written in Go. It's a fork of Clash that supports multiple proxy protocols (VMess, VLESS, Shadowsocks, Trojan, Snell, TUIC, Hysteria, etc.) with advanced routing rules, DNS resolution, and a RESTful API controller.

## Build and Development Commands

### Building
```bash
# Basic build (uses with_gvisor tag by default)
go build -tags with_gvisor

# Build for specific platforms using the Makefile
make linux-amd64-v3    # Linux AMD64 v3
make darwin-arm64       # macOS ARM64
make windows-amd64-v3   # Windows AMD64 v3

# Build all common platforms
make all

# Clean build artifacts
make clean
```

### Testing
```bash
# Run all tests
go test ./...

# Run tests with the gvisor tag
go test ./... -tags "with_gvisor"

# Run a single test
go test -v -run TestFunctionName ./path/to/package

# Run tests in sequence (required for some tests)
go test -p 1 -v ./...
```

### Linting
```bash
# Run linters (gofumpt, staticcheck, govet, gci)
make lint
# or
golangci-lint run ./...

# Test configuration file
go run . -t -f config.yaml
```

### Config Testing
```bash
# Test configuration from file
go run . -t -f config.yaml

# Test configuration from stdin
cat config.yaml | go run . -t -f -

# Test base64-encoded configuration
go run . -t -config <base64>
```

## Go Version Requirements

- Primary development: Go 1.24+
- Minimum supported: Go 1.20
- Repository uses custom Go patches for legacy Windows (7/8) support in `.github/patch/`

## High-Level Architecture

### Core Components

**Main Entry Point** (`main.go`)
- Parses command-line flags and configuration
- Initializes the hub executor and route
- Handles signals (SIGINT, SIGTERM, SIGHUP) for graceful shutdown/reload

**Hub** (`hub/`)
- Central coordinator that ties all components together
- `hub.Parse()` is the main initialization function that:
  1. Parses configuration
  2. Creates the tunnel
  3. Sets up DNS server
  4. Initializes proxies and rules
  5. Starts RESTful API server

**Tunnel** (`tunnel/`)
- Core packet processing engine
- Handles TCP and UDP traffic through queues and worker pools
- Applies routing rules to determine which proxy to use
- Manages connection lifecycle and statistics

**Adapter** (`adapter/`)
- `inbound/`: Protocol listeners (HTTP, SOCKS5, TUN, TPROXY, etc.)
- `outbound/`: Protocol dialers (Shadowsocks, VMess, Trojan, etc.)
- `outboundgroup/`: Proxy groups (URL test, load balance, fallback, selector)
- `provider/`: Dynamic proxy/rule providers

**Listener** (`listener/`)
- Server implementations for each inbound protocol
- Creates listener instances from configuration
- Includes mixed-mode (HTTP+SOCKS) and protocol-specific servers

**DNS** (`dns/`)
- Custom DNS server with DoH/DoT/DoQ support
- Fake IP mode to minimize DNS pollution
- Middleware for response enhancement and filtering
- Supports multiple upstream resolvers

**Rules** (`rules/`)
- Rule matching engine for traffic routing
- Rule types: domain, GEOIP, IPCIDR, process, port, etc.
- Rule providers for dynamic rule sets
- Supports logic rules (AND, OR, NOT operators)

**Config** (`config/`)
- YAML configuration parser
- Validates and initializes all components
- `config.go` contains the main configuration structures

**Transport** (`transport/`)
- Protocol implementations for various proxy protocols
- Each protocol has its own subdirectory
- Includes obfuscation layers and multiplexing

### Request Flow

1. **Inbound**: Listener accepts connection → Creates metadata → Sends to tunnel
2. **Tunnel**: Extracts connection/packet → Matches against rules → Selects proxy
3. **Outbound**: Proxy adapter establishes connection → Relays traffic
4. **Statistics**: Tracks connections, traffic, and errors

### Key Interfaces

- `C.ProxyAdapter`: Outbound proxy interface
- `C.InboundListener`: Inbound listener interface
- `C.Rule`: Routing rule interface
- `C.Tunnel`: Core tunnel interface
- `C.Conn`/`C.PacketConn`: Connection wrappers with tracking

## Configuration Structure

- Main config: `config/config.go` - Defines all configuration structures
- Parsers in `listener/config/` - Protocol-specific inbound config
- Parsers in `adapter/` - Protocol-specific outbound config
- Example configuration: `docs/config.yaml`

## Go Module Structure

- `github.com/metacubex/*` - Forked dependencies with custom modifications
- `github.com/metacubex/sing-*` - Sing-box components
- Many dependencies are pinned for Go 1.20 compatibility

## Important Development Notes

### Platform-Specific Build Tags
- `with_gvisor`: Use gVisor netstack for TUN
- Build tags used extensively for platform-specific code (Go 1.20-1.26, Windows/macOS/Linux variants)

### Windows 7/8 Support
- Requires patching Go runtime with files in `.github/patch/go*.patch`
- Build workflow applies these patches automatically

### Testing Notes
- macOS tests remove `listener/inbound/*_test.go` (networking restrictions)
- Tests run sequentially with `-p 1` flag for certain test suites

### DNS Configuration
- Default resolver is disabled in `main.go` to force use of custom DNS
- `dns/` directory contains all DNS-related code

### State Management
- Use `atomic` package for concurrent state
- `xsync.Map` for concurrent maps
- Connection tracking in `tunnel/statistic/`
