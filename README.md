## What is

This is simple, pure go, console based AI chat with support for mcp in agent mode.

## Features

- Model selection
- Can connect to MCP server currently support stdio. Not tested with tcp or http sse yet.
  I provided one will full tools to enable coding agent mode in mcp-stdio-q36 dir.
  User can implement new one based on the existing pattern.
- Context trimming. Support different model for sumarizing the context (faster)
- Session management, be able to save conversations into file in order to be read by other editor (vim)
- Support multiline input using /edit at prompt
- Simple code, minimum dependencies and ideal for extentions.

## Motivation

I got a small AMD Ryzen 9 PRO 6950H Mini PC--NucBox M7 Pro 32G ram and want to maximize system resource for AI and vibe coding. I want a system lean to minimum.

So I find this work flow very productive.

- Ubuntu 25.10 with Wayland and windows manage is labwc
- No VScode, even not chrome, I can aford one tab Edge browser. Some times I push to the edge of ram usage, no GPU referencing for that hardware, set graphic memory only 1G to maximize RAM
- Development using vim and vim golang inside a tmux session
- Using this ai chat with mcp (trying to do the lack of vscode + continue)
- I can aford these models run with llama-server (build llama.cpp myself to optimize for native CPU)
  - Qwen3-Coder-Next-APEX-I-Mini.gguf 70B
  - gemma-4-26B-A4B-it-UD-Q5_K_XL.gguf
  - Qwen3.6-35B-A3B-Uncensored-HauhauCS-Aggressive-Q4_K_P.gguf

The outcome? Very usable and productive. I got more than 12 to 20 tok/pes, and these three above are great. I can use context size to 64K for gemma and others, for Qwen coder next I can get 24K context which is fine *most of the time* :)

## Features of go-ai-chat

As of now
- Support openai url (llama-server)
- Support mcp server stdio and tcp. Write each mcp server yoursel using the same pattern
- Save AI thiking and answer in a session to a file so u can use vim to open it and read and copy/paste edit etc
- Manage chat session, history

## Quick start

- Clone this repo
- Build the aig cli and the mcp.exe
```
go build -o ~/.local/bin/aig chat/*.go
go build -o mcp.exe mcp-srv/main.go
```

To build with completely portable golang code so you can run it in a chroot environment (to be safe for agent tasks for example)

Most tools are still available except the run_terminal_command - it also works if your chroot has shell and having the required commands.

```
# For fish shell, if u use bash adjust it.
for os in windows linux ; export GOOS=$os && export CGO_ENABLED=0 && export outfile="mcp-$os-amd64.exe" && export buildpath="mcp-srv" && cd $buildpath && go build -trimpath -ldflags="-X main.version=v1.0.1+"(date +'%Y%m%d')" -X main.buildTime="(date +'%Y-%m-%d_%H:%M:%S')" -extldflags=-static -w -s" --tags "osusergo,netgo" -o ../$outfile . ; end

for os in windows linux ; export GOOS=$os && export CGO_ENABLED=0 && export outfile="aig-$os-amd64.exe" && export buildpath="chat" && cd $buildpath && go build -trimpath -ldflags="-X main.version=v1.0.1+"(date +'%Y%m%d')" -X main.buildTime="(date +'%Y-%m-%d_%H:%M:%S')" -extldflags=-static -w -s" --tags "osusergo,netgo" -o ../$outfile . ; end

# Create a root dir
mkdir my-mcp-root
# Copy binaries over to the root
# Create minimum network files
mkdir my-mcp-root/etc
echo "nameserver 8.8.8.8" > my-mcp-root/etc/resolve.conf
echo "127.0.0.1 localhost" > my-mcp-root/etc/hosts

chroot my-mcp-root ./aig-linux-amd64.exe
```

- Run the `aig` and answer the first prompts
- Select the model properly
- after startup load the mcp using command `/mcp ./mcp-linux-amd64.exe`
- Ask for news headline (output below is shorten) `fetch a google news and show me news headline`

```
✅ New context started.
> /mcp ./mcp.exe
🔌 Disconnecting previous MCP server...
🚀 Launching MCP stdio server: ./mcp.exe
✅ MCP connected: ./mcp.exe
   1 tool(s) available:
   • fetch_url — Fetches a URL over HTTP and converts its HTML content into markdown te
xt.
> fetch a google news and show me news headline

> 🔧 Planning tool call: fetch_url

🔧 Calling tool: fetch_url
   args: {"url":"https://news.google.com"}

> 📝 Response:

> 📝 Response:                                                                         Here are the top news headlines from Google News:

### Top Stories
                                                                                       1.  **Shots fired at White House Correspondents' Dinner**
    *Source: abc.net.au*

### Other Headlines

*   **Ticket to ride: Australian IS brides secure flights home**
    *Source: The Age*

### Top Stories

1.  **Shots fired at White House Correspondents' Dinner**

### Other Headlines

*   **Ticket to ride: Australian IS brides secure flights home**
    *Source: The Age*

```
## Quick doc

the help should show but here is the command. Commnd started with / ; otherwise it is the message to be sent to AI.

/new <context-name> Make a new context name. With thout context-name the name will take the first some characters of the first question.

a context is a conversation session. To list run /l or /list. To use a old session type /use <context-name> . Note that it will include the old conversations.

while you are chatting, if you want to see the list of questions and answer from ai from the current session run /h or /history.

/add <file> - Add a file to the current context. Accept text/image files.

/r <cmd> - Run ssytem command - the output will be printed on the screen

/exit - exit

/m <model> - Change model name. To know what current is run /p

/url <url> - Set API URL

/del <context> - delete context

/timeout <dur> - Set timeout when talking to AI timeout

/mcp <spec> - Connect to a mcp server. if it starts with tcp:// then connect via tcp. Otherwise it will run the command path.
<spec> can have some sub command
  - docs

/edit - this will start a editor to type multi line message.

## What next

You can read the code and make new mcp tools for your goal or do anything with it. Code is pure go, reflecting the JsonRPC 2 protocol.

The code is written using vibe coding and I use Qwen coder next, gemma4 and Qwen3.6 to get into this state so far in around 2 days. Just need to guide them a bit about JsonRPC and mcp specs (only Qwen3.6 know all specs of servers, gemma4 and other miss one or two thus the server is not kind of complete but it is doable)

I intend to use pure go rather than other go ai sdk.

But eventually I use "github.com/mark3labs/mcp-go/mcp" for the server part in one of the mcp code!

Have fun!

## Status

- CLI chat/*.go
Stable, most features completed.

- MCP server is tested and best with mcp-srv/main.go . Users can make more and more to suit their case.

## Issues

- Some times Qwen3.6 stop in the middle of something.

There are a lots of efforts to bypass, ignore some strange `think outloud` from Qween! However it may still happen. You just need to type continue to make the model continue. It is not just this prog, many other ai gant has the similar like this as well eg. vscode continue, it is just the model producing some quirky texts at times
