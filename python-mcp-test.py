import subprocess
import json
import time

# Path to your compiled Go binary
SERVER_BIN = "./mcp-server"

def run_test():
    print(f"Starting stress test against {SERVER_BIN}...\n")

    process = subprocess.Popen(
        [SERVER_BIN],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1
    )

    def send_request(method, params, req_id):
        request = {
            "jsonrpc": "2.0",
            "id": req_id,
            "method": method,
            "params": params
        }
        print(f"[CLIENT] Sending: {method} (ID: {req_id})")
        process.stdin.write(json.dumps(request) + "\n")
        process.stdin.flush()

        line = process.stdout.readline()
        if not line:
            print("[ERROR] No response from server (EOF)")
            return None

        try:
            return json.loads(line)
        except json.JSONDecodeError:
            print(f"[ERROR] Failed to decode JSON. Raw line: {line}")
            return None

    try:
        # 1. Test Initialization
        print("--- Test 1: Initialize ---")
        res = send_request("initialize", {}, 1)
        if res and 'result' in res:
            print(f"[SERVER] Success: {res['result'].get('serverInfo', {}).get('name')}")
        else:
            print(f"[SERVER] Failed: {res}")

        # 2. Test List Tools
        print("\n--- Test 2: List Tools ---")
        res = send_request("tools/list", {}, 2)
        if res and 'result' in res and 'tools' in res['result']:
            print(f"[SERVER] Success: Found {len(res['result']['tools'])} tools")
        else:
            print(f"[SERVER] FAILED! Full Response: {res}")

        # 3. Test Valid Command (OK)
        print("\n--- Test 3: Valid Command (list_directory) ---")
        res = send_request("tools/call", {
            "name": "list_directory",
            "arguments": {"path": "."}
        }, 3)
        if res and 'result' in res:
            content = res['result']['content'][0]['text']
            print(f"[SERVER] Success! Content preview: {content[:50].strip()}...")
        else:
            print(f"[SERVER] FAILED: {res}")

        # 4. Test Expected Error (Invalid Path)
        print("\n--- Test 4: Expected Error (Non-existent path) ---")
        res = send_request("tools/call", {
            "name": "list_directory",
            "arguments": {"path": "/non/existent/path/12345"}
        }, 4)
        if res and 'error' in res:
            print(f"[SERVER] Error caught as expected: {res['error']['message']}")
        else:
            print(f"[SERVER] FAILED: Unexpected success or format: {res}")

        # 5. THE BIG TEST: Trigger Panic (The "Crash" Simulator)
        print("\n--- Test 5: THE CRASH TEST (chaos_panic) ---")
        res = send_request("tools/call", {
            "name": "chaos_panic",
            "arguments": {}
        }, 5)
        if res and 'error' in res and "Recovered" in res['error']['message']:
            print("✅ SUCCESS: The server recovered from the panic!")
        else:
            print(f"❌ FAILED: Server did not return recovery error. Response: {res}")

        # 6. Test if server is still alive after panic
        print("\n--- Test 6: Post-Panic Survival Check ---")
        res = send_request("tools/call", {
            "name": "list_directory",
            "arguments": {"path": "."}
        }, 6)
        if res and 'result' in res:
            print("✅ SUCCESS: Server is still responding to commands!")
        else:
            print(f"❌ FAILED: Server is dead after the panic. Response: {res}")

    except Exception as e:
        print(f"\n[!!!] Python Script Error: {e}")
    finally:
        print("\nCleaning up...")
        process.terminate()

if __name__ == "__main__":
    run_test()

