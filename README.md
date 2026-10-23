# WhatsApp MCP Server

This is a Model Context Protocol (MCP) server for WhatsApp.

With this you can search and read your personal Whatsapp messages (including images, videos, documents, and audio messages), search your contacts and send messages to either individuals or groups. You can also send media files including images, videos, documents, and audio messages.

It connects to your **personal WhatsApp account** directly via the Whatsapp web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). All your messages are stored locally in a SQLite database and only sent to an LLM (such as Claude) when the agent accesses them through tools (which you control).

Here's an example of what you can do when it's connected to Claude.

![WhatsApp MCP](./example-use.png)

> To get updates on this and other projects I work on [enter your email here](https://docs.google.com/forms/d/1rTF9wMBTN0vPfzWuQa2BjfGKdKIpTbyeKxhPMcEzgyI/preview)

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). This means that prompt injection could lead to private data exfiltration.

## Security-hardened fork

This fork fixes the issues documented in [SECURITY_AUDIT.md](./SECURITY_AUDIT.md). The most important ones were an unauthenticated bridge API reachable from the network and from any website, arbitrary file upload, and a path traversal in media downloads. In short:

- The bridge listens on `127.0.0.1` only, and every request needs a random API token (created in `whatsapp-bridge/store/api_token` on first run and read automatically by the MCP server). Browser and DNS-rebinding requests are refused.
- Files can only be sent from `whatsapp-bridge/store/outbox/` (or directories you add with `WHATSAPP_MEDIA_ALLOWED_DIRS`). Hidden files and the bridge's own secrets are always refused.
- Downloads are written to `whatsapp-bridge/store/media/<chat>/<type>_<message id>.<ext>`; the sender's filename can no longer choose the path.
- The store is private to your user (`0700`/`0600`), and message text is no longer written to logs.
- Tools return JSON, strip control characters, and tell the model that message content is untrusted. Send tools are marked destructive so your MCP client can ask before each send.
- Optional guardrails: a recipient allowlist, a read-only mode and a send rate limit (see [Configuration](#configuration)).

**For the strongest protection against prompt injection**, set `WHATSAPP_ALLOWED_RECIPIENTS` or `WHATSAPP_READ_ONLY=1`, and keep per-call approval for the `send_*` tools in your MCP client.

## Installation

### Prerequisites

- Go 1.26+ (older Go versions download the required toolchain automatically)
- Python 3.11+
- Anthropic Claude Desktop app (or Cursor)
- UV (Python package manager), install with `curl -LsSf https://astral.sh/uv/install.sh | sh`
- FFmpeg (_optional_) - Only needed for audio messages. If you want to send audio files as playable WhatsApp voice messages, they must be in `.ogg` Opus format. With FFmpeg installed, the MCP server will automatically convert non-Opus audio files. Without FFmpeg, you can still send raw audio files using the `send_file` tool.

### Steps

1. **Clone this repository**

   ```bash
   git clone https://github.com/lharries/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Run the WhatsApp bridge**

   Navigate to the whatsapp-bridge directory and run the Go application:

   ```bash
   cd whatsapp-bridge
   go run main.go
   ```

   The first time you run it, you will be prompted to scan a QR code. Scan the QR code with your WhatsApp mobile app to authenticate.

   On start-up the bridge creates `store/api_token`, `store/outbox/` and `store/media/`, and restricts the `store` directory to your user. The REST API is only available on `127.0.0.1:8080`.

   After approximately 20 days, you will might need to re-authenticate.

3. **Connect to the MCP server**

   Copy the below json with the appropriate {{PATH}} values:

   ```json
   {
     "mcpServers": {
       "whatsapp": {
         "command": "{{PATH_TO_UV}}", // Run `which uv` and place the output here
         "args": [
           "--directory",
           "{{PATH_TO_SRC}}/whatsapp-mcp/whatsapp-mcp-server", // cd into the repo, run `pwd` and enter the output here + "/whatsapp-mcp-server"
           "run",
           "main.py"
         ],
         "env": {
           // Optional guardrails, see "Configuration" below
           "WHATSAPP_ALLOWED_RECIPIENTS": "447700900123,120363025246125486@g.us"
         }
       }
     }
   }
   ```

   The MCP server enforces the allowlist and read-only mode itself. Set them for the bridge too (in the environment where you run `go run main.go`), so they hold even if something else gets hold of the API token.

   For **Claude**, save this as `claude_desktop_config.json` in your Claude Desktop configuration directory at:

   ```
   ~/Library/Application Support/Claude/claude_desktop_config.json
   ```

   For **Cursor**, save this as `mcp.json` in your Cursor configuration directory at:

   ```
   ~/.cursor/mcp.json
   ```

4. **Restart Claude Desktop / Cursor**

   Open Claude Desktop and you should now see WhatsApp as an available integration.

   Or restart Cursor.

### Windows Compatibility

If you're running this project on Windows, be aware that `go-sqlite3` requires **CGO to be enabled** in order to compile and work properly. By default, **CGO is disabled on Windows**, so you need to explicitly enable it and have a C compiler installed.

#### Steps to get it working:

1. **Install a C compiler**  
   We recommend using [MSYS2](https://www.msys2.org/) to install a C compiler for Windows. After installing MSYS2, make sure to add the `ucrt64\bin` folder to your `PATH`.  
   → A step-by-step guide is available [here](https://code.visualstudio.com/docs/cpp/config-mingw).

2. **Enable CGO and run the app**

   ```bash
   cd whatsapp-bridge
   go env -w CGO_ENABLED=1
   go run main.go
   ```

Without this setup, you'll likely run into errors like:

> `Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work.`

## Configuration

All settings are environment variables. Set bridge settings where you run `go run main.go`, and MCP server settings in the `env` block of your MCP client config. Where both are listed, set the same value in both places.

| Variable | Used by | Default | Purpose |
|---|---|---|---|
| `WHATSAPP_ALLOWED_RECIPIENTS` | both | unset (any individual or group) | Comma-separated phone numbers / JIDs that may receive messages. Strongly recommended. |
| `WHATSAPP_READ_ONLY` | both | off | `1` disables sending: the MCP server hides the `send_*` tools, and the bridge refuses sends. |
| `WHATSAPP_MEDIA_ALLOWED_DIRS` | both | only `store/outbox` | Extra absolute directories (separated by `:` on macOS/Linux or `;` on Windows) from which files may be sent. |
| `WHATSAPP_SEND_RATE_PER_MINUTE` | bridge | `10` | Maximum sends per minute (`0` = unlimited). |
| `WHATSAPP_MAX_MEDIA_MB` | both | `100` | Maximum size of media sent or downloaded. |
| `WHATSAPP_STORE_DIR` | both | `whatsapp-bridge/store` | Location of the session, database, token, outbox and media. Use the same absolute path for both. |
| `WHATSAPP_BRIDGE_ADDR` | bridge | `127.0.0.1:8080` | Listen address; must be a loopback address. |
| `WHATSAPP_BRIDGE_URL` | MCP server | `http://127.0.0.1:8080/api` | Bridge URL; must be loopback `http://`. |
| `WHATSAPP_BRIDGE_TOKEN` | both | read from `store/api_token` | Override the API token (at least 32 characters). |

### Upgrading from upstream

- Your existing login (`store/whatsapp.db`) and message history keep working. The bridge tightens file permissions and adds a `direct_path` column on first start.
- To send a file, copy it into `whatsapp-bridge/store/outbox/` first (or allow its directory with `WHATSAPP_MEDIA_ALLOWED_DIRS`).
- New downloads go to `store/media/`. Files downloaded by the old version stay in `store/<chat jid>/`.
- Tool results are now JSON objects instead of preformatted text.

## Architecture Overview

This application consists of two main components:

1. **Go WhatsApp Bridge** (`whatsapp-bridge/`): A Go application that connects to WhatsApp's web API, handles authentication via QR code, and stores message history in SQLite. It serves as the bridge between WhatsApp and the MCP server.

2. **Python MCP Server** (`whatsapp-mcp-server/`): A Python server implementing the Model Context Protocol (MCP), which provides standardized tools for Claude to interact with WhatsApp data and send/receive messages.

### Data Storage

- All message history is stored in a SQLite database within the `whatsapp-bridge/store/` directory
- The database maintains tables for chats and messages
- Messages are indexed for efficient searching and retrieval
- `store/whatsapp.db` holds your linked-device keys: anyone with a copy can act as this device. Never share or commit the `store` directory, and unlink the device from your phone (Settings > Linked Devices) if it leaks

## Usage

Once connected, you can interact with your WhatsApp contacts through Claude, leveraging Claude's AI capabilities in your WhatsApp conversations.

### MCP Tools

Claude can access the following tools to interact with WhatsApp:

- **search_contacts**: Search for contacts by name or phone number
- **list_messages**: Retrieve messages with optional filters and context
- **list_chats**: List available chats with metadata
- **get_chat**: Get information about a specific chat
- **get_direct_chat_by_contact**: Find a direct chat with a specific contact
- **get_contact_chats**: List all chats involving a specific contact
- **get_last_interaction**: Get the most recent message with a contact
- **get_message_context**: Retrieve context around a specific message
- **send_message**: Send a WhatsApp message to a specified phone number or group JID
- **send_file**: Send a file (image, video, raw audio, document) to a specified recipient
- **send_audio_message**: Send an audio file as a WhatsApp voice message (requires the file to be an .ogg opus file or ffmpeg must be installed)
- **download_media**: Download media from a WhatsApp message and get the local file path

### Media Handling Features

The MCP server supports both sending and receiving various media types:

#### Media Sending

You can send various media types to your WhatsApp contacts:

- **Images, Videos, Documents**: Use the `send_file` tool to share any supported media type. The file must be in `whatsapp-bridge/store/outbox/` or a directory listed in `WHATSAPP_MEDIA_ALLOWED_DIRS`.
- **Voice Messages**: Use the `send_audio_message` tool to send audio files as playable WhatsApp voice messages.
  - For optimal compatibility, audio files should be in `.ogg` Opus format.
  - With FFmpeg installed, the system will automatically convert other audio formats (MP3, WAV, etc.) to the required format.
  - Without FFmpeg, you can still send raw audio files using the `send_file` tool, but they won't appear as playable voice messages.

#### Media Downloading

By default, just the metadata of the media is stored in the local database. The message will indicate that media was sent. To access this media you need to use the download_media tool which takes the `message_id` and `chat_jid` (which are shown when printing messages containing the media), this downloads the media to `whatsapp-bridge/store/media/` and then returns the file path which can be then opened or passed to another tool. Treat downloaded files as untrusted: they were sent by someone else.

## Technical Details

1. Claude sends requests to the Python MCP server
2. The MCP server queries the Go bridge for WhatsApp data or directly to the SQLite database
3. The Go accesses the WhatsApp API and keeps the SQLite database up to date
4. Data flows back through the chain to Claude
5. When sending messages, the request flows from Claude through the MCP server to the Go bridge and to WhatsApp

## Troubleshooting

- If you encounter permission issues when running uv, you may need to add it to your PATH or use the full path to the executable.
- Make sure both the Go application and the Python server are running for the integration to work properly.
- **"Bridge API token not found" / "missing or invalid API token"**: start the bridge once so it creates `store/api_token`. If you moved the store, set `WHATSAPP_STORE_DIR` to the same absolute path for the bridge and the MCP server.
- **"media_path is outside the allowed media directories"**: copy the file into `whatsapp-bridge/store/outbox/`, or add its directory to `WHATSAPP_MEDIA_ALLOWED_DIRS` for both processes.
- **"recipient is not listed in WHATSAPP_ALLOWED_RECIPIENTS"**: add the number or group JID to the allowlist in the bridge's environment and restart it.

### Authentication Issues

- **QR Code Not Displaying**: If the QR code doesn't appear, try restarting the authentication script. If issues persist, check if your terminal supports displaying QR codes.
- **WhatsApp Already Logged In**: If your session is already active, the Go bridge will automatically reconnect without showing a QR code.
- **Device Limit Reached**: WhatsApp limits the number of linked devices. If you reach this limit, you'll need to remove an existing device from WhatsApp on your phone (Settings > Linked Devices).
- **No Messages Loading**: After initial authentication, it can take several minutes for your message history to load, especially if you have many chats.
- **WhatsApp Out of Sync**: If your WhatsApp messages get out of sync with the bridge, delete both database files (`whatsapp-bridge/store/messages.db` and `whatsapp-bridge/store/whatsapp.db`) and restart the bridge to re-authenticate.

For additional Claude Desktop integration troubleshooting, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues). The documentation includes helpful tips for checking logs and resolving common issues.
