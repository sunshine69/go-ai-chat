👥 The Connection Flow[ Chat Client ]                                      [ Llama Server ]

       |                                                    |
       | ------ 1. POST /v1/chat/completions (Stream) ----> |
       | <----- 2. HTTP 200 OK (text/event-stream) -------- |
       |                                                    |
       | <----- 3. data: {"content": "Hello"} ------------- |
       | <----- 4. data: {"content": " world"} ------------ |
       | <----- 5. data: [DONE] --------------------------- |

**The Request**: You type a message and hit send. The client makes a standard HTTP request to the Llama server. The request includes a setting that asks for a stream, like "stream": true.The Response Start: The server accepts the request. It sends back a special HTTP header: Content-Type: text/event-stream. This tells the client to keep the connection open for a live stream of data.The Stream: The Llama model thinks and creates words one by one. As each piece of text is ready, the server sends it down the open pipe.The End: When the AI finishes its sentence, the server sends a final message to say it is done. Then, the connection closes.

📝 The Data FormatThe data sent over SSE must follow a strict text format. Each message starts with the word data:, followed by the text, and ends with two newlines.A real stream from a Llama server looks like this:

```
data: {"choices": [{"delta": {"content": "Artificial"}}]}
data: {"choices": [{"delta": {"content": " Intelligence"}}]}
data: {"choices": [{"delta": {"content": " is"}}]}
data: [DONE]

// Streaming Reasoning Content (Thinking Mode)
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1716450000,"model":"llama3-reasoning","choices":[{"index":0,"delta":{"reasoning_content":"Let me calculate the square root..."},"finish_reason":null}]}

//Streaming a Tool Call (The Function Name)When the model transitions to calling a tool, it emits the tool index, call ID, and the name of the function:

data: {"id":"chatcmpl-124","object":"chat.completion.chunk","created":1716450001,"model":"llama3-tools","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_987","type":"function","function":{"name":"fetch_stock_price","arguments":""}}]},"finish_reason":null}]}

// Streaming Tool Arguments (Chunk-by-Chunk)
data: {"id":"chatcmpl-124","object":"chat.completion.chunk","created":1716450002,"model":"llama3-tools","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ticker\":"}}]},"finish_reason":null}]}
// (The next chunk might just be {"arguments":"\"AAPL\"}"}).

// The Closing Chunk (Signal to Stop)

data: {"id":"chatcmpl-124","object":"chat.completion.chunk","created":1716450003,"model":"llama3-tools","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

```

The agurments does not need to be full json in one line, ai can send it in fragments (my fault reading before) - 
