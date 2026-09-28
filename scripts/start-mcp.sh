#!/bin/sh

MCP="${MCP:-$1}"

mkdir -p .task_caches/fake_home

bwrap \
  --dir /tmp \
  --dev /dev \
  --proc /proc \
  --bind "$(pwd)" "$(pwd)" \
  --bind "$(pwd)/.task_caches/fake_home" "/tmp/$(whoami)" \
  --setenv HOME "/tmp/$(whoami)" \
  --setenv PATH "/home/stevek/.local/bin:/usr/local/bin:/usr/bin:/usr/sbin:/bin:/sbin" \
  --ro-bind / / \
  --chdir "$(pwd)" \
  mcp.exe -tools all -t streamable -p 8081
  # $MCP -tools all -t streamable -p 8081
