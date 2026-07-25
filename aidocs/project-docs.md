# Project Documentation

## Overview
- This is a simple Go-based CLI chat tool with MCP (Model Context Protocol) support
- Uses llama-server for local model inference on AMD Ryzen 9 PRO 6950H Mini PC
- Supports OpenAI-compatible API endpoints and MCP stdio/TCP servers

## System Requirements
- Ubuntu 24.04+ with Wayland + labwc window manager
- No GUI apps (no VSCode, minimal browser usage)
- Development in vim/tmux sessions
- ~32GB RAM allocated for CPU-based inference

## Models Available
| Model | Context Size | Notes |
|-------|-------------|-------|
| gemma-4-26B-A4B-it-UD-Q5_K_XL | 24K | Good general purpose |
| Qwen3.6-35B-A3B-Uncensored | 24K | Uncensored variant |

Performance: ~12-20 tok/s on native CPU builds of llama.cpp

## Quick Start
```bash
# Build the CLI and MCP server
go build -o ~/.local/bin/aig chat/*.go
go build -o mcp.exe mcp-stdio-go/main.go

# Run interactive mode
aig

# Non-interactive one-shot in a new session with mcp server
aig /mcp ./mcp.exe /n /q your question here
```

## Commands
| Command | Description |
|---------|-------------|
| `/new` or `/n` | Start new context/session |
| `/add <file>` | Add file contents to context |
| `/r <cmd>` | Run shell command, show output |
| `/m <model>` | Switch model |
| `/url <url>` | Change API endpoint |
| `/timeout <dur>` | Set request timeout (e.g., 30s, 5m) |
| `/exit` or `/q` | Exit REPL |
| `/history` or `/h` | Show current session history |
| `/list` or `/l` | List saved contexts for current model |
| `/use <name>` | Switch to existing context |
| `/del <name>` | Delete a context |
| `/mcp <spec>` | Connect MCP server (tcp://host:port or cmd path) |
| `/mcp off` | Disconnect MCP |
| `/mcp tools` | List available MCP tools |
| `/edit` | Open editor for multi-line input |

## Configuration
Settings are stored in `~/.aigdotenv`:
- `OPENAI_URL` - API endpoint URL
- `OPENAI_MODEL` - Default model name
- `OPENAI_API_KEY` - API key (if needed)
- `TIMEOUT` - Request timeout duration
- `SHOW_THINKING=true/false` - Show reasoning content

## Session Management
Sessions are stored in `~/.aig/` as JSON files. Each session is named by timestamp + query summary + model name.

---
*This doc was auto-generated from README.md and project structure.*
