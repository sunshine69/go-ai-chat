#!/bin/bash

# This script will keep restarting the MCP server if it exits.
# It logs errors to a local file so you can debug why it crashed.

LOG_FILE="mcp_server_error.log"

while true; do
    echo "$(date): Starting MCP Server..." >> "$LOG_FILE"

    # Replace './mcp-server' with the actual path to your compiled binary
    ./mcp.exe 2>> "$LOG_FILE"

    EXIT_CODE=$?
    echo "$(date): Server exited with code $EXIT_CODE. Restarting in 2 seconds..." >>
 "$LOG_FILE"

    # Wait a moment before restarting to prevent infinite high-speed loop if there'sa constant error
    sleep 2
done

