# Testing Multi-Workdir Streamable Mode

## Build and Start Server

```bash
cd mcp-srv
go build -o /tmp/mcp-server-test .
/tmp/mcp-server-test -t streamable -p 8081 -work-dir .
```

## Test with curl (Manual Testing)

### 1. Test creating a session with workdir "test1"
```bash
curl -X POST http://localhost:8081/mcp/test1 \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
      "tool_name": "list_directory",
      "arguments": {"path": "."}
    },
    "id": 1
  }'
```

### 2. Test creating a session with workdir "test2"
```bash
curl -X POST http://localhost:8081/mcp/test2 \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
      "tool_name": "list_directory",
      "arguments": {"path": "."}
    },
    "id": 2
  }'
```

### 3. Test that accessing "test1" again is rejected
```bash
curl -X POST http://localhost:8081/mcp/test1 \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
      "tool_name": "list_directory",
      "arguments": {"path": "."}
    },
    "id": 3
  }'
```
Expected: Error message about workdir being occupied

### 4. Test creating a file in "test1"
```bash
curl -X POST http://localhost:8081/mcp/test1 \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
      "tool_name": "create_new_file",
      "arguments": {"path": "myfile.txt", "content": "Hello from test1"}
    },
    "id": 4
  }'
```

### 5. Test that "test2" doesn't see the file from "test1"
```bash
curl -X POST http://localhost:8081/mcp/test2 \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "tools/call",
    "params": {
      "tool_name": "read_file",
      "arguments": {"path": "myfile.txt"}
    },
    "id": 5
  }'
```
Expected: File not found error (test2's workdir is separate)

## Expected Behavior

1. Each workdir gets its own isolated working directory
2. Workdirs are created on demand
3. Only one client can access a workdir at a time
4. Different workdirs are completely isolated from each other