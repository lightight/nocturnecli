# ◗ Nocturne

A terminal coding agent — like Claude Code, powered by the [Nocturne API](https://nocturne.lol).
It reads and edits files, runs commands, searches your codebase, and accepts pasted
images, all from a single cross-platform binary.

```
╭────────────────────────────────────────────────────────╮
│  ◗ Nocturne  coding agent · v0.3.0                      │
│                                                         │
│  model  navy:gpt-5.5                                    │
│  cwd    ~/projects/my-app                               │
╰─────────────────────────────────────────────────────────╯
› build a small http server in main.go and run it
● write_file(main.go)
  └ Wrote 412 bytes to main.go
● run_command go run main.go &
  └ listening on :8080
```

## Install

**macOS / Linux**

```sh
curl -fsSL https://nocturnecode.lol/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://nocturnecode.lol/install.ps1 | iex
```

The hosted installer grabs a prebuilt binary for your OS/arch from
`nocturnecode.lol/bin/`, falls back to GitHub Releases, then falls back to building
from source if Go is available. Override the source repo with
`NOCTURNE_REPO=you/your-fork` and the install location with
`NOCTURNE_INSTALL_DIR`.

**From source** (Go 1.26+):

```sh
git clone https://github.com/lightight/nocturnecli && cd nocturnecli
make install        # -> ~/.local/bin/nocturne
# or: go build -o nocturne . && ./nocturne
```

## Setup

Nocturne needs an API key. Get one from your [Nocturne account page](https://nocturne.lol/account.html),
then launch Nocturne and save it once with `/key`:

```sh
nocturne
› /key noct_your_key
```

The key is written to `~/.config/nocturne/config.json` (macOS: `~/Library/Application Support/nocturne`),
with file permissions `0600`, and reused for every project on your machine. No `export`, no per-folder
`.env`. A bare `/key` also works: it saves whatever key is already loaded.

If you'd rather manage the key yourself, Nocturne still reads `NOCTURNE_API` from a local `.env` file or
the environment. Precedence is: an exported environment variable first, then the saved config, then a
local `.env` — so a stale project `.env` can never silently override the key you saved with `/key`.

Nocturne verifies the key on startup. If it has expired or stopped working you'll be told to create a
new one at [nocturne.lol/account](https://nocturne.lol/account) — then save it with `/key` as above.

The default model is **`navy:gpt-5.5`**; change it with `/model <id>` in-app or `-m` on the command
line. Model availability is per-account, so use an id you've been granted.

## Usage

```sh
nocturne                       # interactive TUI
nocturne cowork                # interactive TUI in cowork (computer-use) mode
nocturne -p "fix the test in foo_test.go"   # one-shot, non-interactive (auto-runs tools)
nocturne cowork -p "organize my ~/Downloads"  # one-shot with computer-use tools
nocturne -m some-model         # override the model for this run
nocturne update                # self-update to the latest release
nocturne update --check        # check for a newer release without installing
nocturne serve --addr :8080 --bin ./dist  # host docs, installers, and relay
nocturne --help
```

Replies **stream live** by default — tokens appear as the model writes them, then
settle into the final formatted answer. Toggle it with `/stream`.

### In-app commands

| Command          | Does                                            |
| ---------------- | ----------------------------------------------- |
| `/help`          | list commands                                   |
| `/model [id]`    | open the model picker, or set a model by id     |
| `/models`        | list the models your account can use            |
| `/level`         | thinking level: `off` · `normal` · `extended`   |
| `/key [noct_…]`  | save your API key to the private config (remembered everywhere) |
| `/image <path>`  | attach an image file (or `Ctrl+V` for the clipboard) |
| `/auto`          | toggle auto-accept for edits & commands         |
| `/stream`        | toggle live response streaming (on by default)  |
| `/cd <dir>`      | change the working directory                    |
| `/cowork`        | toggle computer-use mode (screen control + whole filesystem) |
| `/plan`          | plan mode — explore read-only, run again to approve & execute |
| `/tokens`        | show token usage, context size & quota          |
| `/compact`       | summarize the conversation to free up context   |
| `/resume`        | resume a saved chat from this directory         |
| `/new`           | start a new chat                                |
| `/remote`        | control this session from your browser (E2E-encrypted) |
| `/clear`         | clear the conversation (starts a new session)   |
| `/init`          | generate a `NOCTURNE.md` for the project        |
| `/update`        | update Nocturne to the latest release           |
| `/exit`          | quit (`Ctrl+C` also works)                      |

### Keys

- **Enter** — send · **Alt+Enter** — newline · **Ctrl+V** — paste a clipboard image
- **PgUp/PgDn** — scroll the transcript · **Esc** — interrupt a running request · **Ctrl+C** — quit
- Type **`/`** to open the command menu; **↑/↓** to move, **Tab** to complete, **Enter** to run.

Nocturne runs as a full-screen TUI and reflows when you resize the terminal.

### Images

Attach an image three ways:

- press **Ctrl+V** to grab an image off the system clipboard,
- `/image diagram.png`, or
- drag a file into the terminal / mention a path inline, e.g. `explain ./diagram.png`.

**The model can actually see the image when you're on a vision-capable model** (marked
`vision` in `/model` / `/models`, e.g. `navy:claude-haiku-4.5`, `navy:gpt-5.4-mini`,
`navy:gpt-5.5`, `navy:gemini-3.5-flash`). On a non-vision model the image still attaches, and Nocturne
has a vision model describe it before sending that description as text.

> Clipboard image support uses native helpers: `pngpaste`/AppleScript on macOS,
> `wl-paste`/`xclip` on Linux, and PowerShell on Windows.

### Cowork (computer use)

Cowork mode lets Nocturne work the whole computer, not just the project directory — inspired by
desktop agents that see the screen and act on it. Start it with `nocturne cowork`, toggle it
mid-session with `/cowork`, or just ask for something that needs it: the agent can request cowork
mode itself, and you approve once.

In cowork mode the agent can:

- **see the screen** — `screenshot` captures the display and a vision model reads it,
- **drive the GUI** — click, move, scroll, type, and press keys (each action needs your approval
  unless you've enabled auto-accept),
- **open things** — apps, files, folders, and URLs,
- **navigate the whole filesystem** — absolute paths anywhere, not just the working directory.

Screen work needs the **Accessibility permission** on macOS (System Settings → Privacy & Security →
Accessibility) and **Screen Recording** for screenshots. Screenshots
report the screen size in points vs. pixels so click coordinates land correctly on Retina displays. On a
non-vision model the capture is described by a vision model as text (clickable elements included, with
pixel coordinates) — clicking, typing, and scrolling work the same either way, since they take plain
JSON arguments.

> Backends: `screencapture` + CoreGraphics/System Events on macOS, PowerShell on Windows,
> and a built-in pure-Go X11 backend on Linux — no extra packages to install on any platform
> (`xdotool` + `grim`/`scrot`/ImageMagick are used only as fallbacks when no X server is
> reachable, e.g. a native Wayland session without XWayland).

### Sessions & context

- Chats are **saved automatically**, scoped to the directory they ran in. **`/resume`** opens a
  picker of saved chats from the current directory and restores one (messages, model, transcript).
- **`/compact`** summarizes the conversation into a compact brief and continues from it, freeing
  up context. The summary is kept silently as context — it isn't dumped into the chat. Nocturne
  also **compacts automatically** as the context approaches ~1M tokens. `/tokens` shows the live
  context size.
- **Scroll** the transcript with **PgUp/PgDn** or the mouse wheel (most terminals map the wheel
  to scrolling automatically). Text selection, copy, and paste always work natively — mouse
  capture is off by default; run `/mouse` only if your terminal's wheel doesn't scroll. The input
  box **wraps** long prompts onto new lines (it grows as you type) instead of scrolling text off.
- While a reply streams, in-progress tool calls show as a tidy `● preparing tool call…` line
  rather than raw `<tool …>` text.

### Remote control (`/remote`)

Run **`/remote`** to drive the session from a phone or another computer. Nocturne
registers an end-to-end encrypted session with the hosted relay and prints a
public URL + a 6-character pairing code:

```
Remote control · end-to-end encrypted
  open  https://nocturnecode.lol/r/m6x8q2p4bkx7y
  code  DNUH9W
```

Open the URL on any device and enter the code. You can then send messages and
watch replies (and tool activity) stream in live. The browser box mirrors the
terminal's: **slash commands autocomplete** in a menu (`/model`, `/plan`,
`/compact`, …) and run in the session, the **draft is synced both ways** as you
type, and while the agent is working the Send button becomes a **Stop** button
that interrupts it. Everything between the browser
and the terminal is **end-to-end encrypted** with AES-256-GCM using a key derived
from the pairing code (PBKDF2). The pairing code never crosses the wire; the
relay only sees opaque ciphertext, and a wrong code simply can't decrypt
anything. Run `/remote off` to stop. Tool approvals still happen at the terminal,
so the human at the keyboard stays in control.

Use `NOCTURNE_RELAY=https://your-host.example` to point `/remote` at a different
relay while developing or self-hosting.

## Hosting

`nocturne serve` runs the docs site, hosted install scripts, optional binary
downloads, and the encrypted remote-control relay:

```sh
make dist
nocturne serve --addr :8080 --bin ./dist
```

Put it behind HTTPS at `nocturnecode.lol` with your reverse proxy of choice. The
browser remote client needs HTTPS for Web Crypto, and the server honors
`X-Forwarded-Proto: https` when it templates the hosted installer URLs.

### Models, thinking & quota

- **`/model`** opens an arrow-key picker of every model your account can use, with pricing
  and `reasoning`/`vision` tags; `/model <id>` sets one directly. `/models` (or
  `nocturne models`) lists them. The list comes from `GET /api/ai/config`.
- **`/level off|normal|extended`** controls how much reasoning models think before answering.
  `extended` also unlocks the **`task` tool**: the model can delegate a self-contained
  sub-task to a sub-agent with its own agent loop and full tool access (aliases: `agent`,
  `subagent`). You approve the delegation once; inside the sub-agent tools run without
  further prompts, and its final report comes back as the tool result. Sub-agents run at
  extended thinking and can't spawn sub-agents of their own.
- **`/tokens`** shows session tokens, your daily quota (used / cap / remaining, straight from
  the `quota` object the API returns on every call), and the current model + thinking level.

## How it works

The Nocturne endpoint is a plain completion API with no native function-calling, so
Nocturne drives tools with a small prompt protocol: the model emits
`<tool name="…">{json}</tool>` blocks, the CLI executes them (`read_file`, `write_file`,
`edit_file`, `list_dir`, `search`, `run_command`), feeds the results back wrapped in
`<tool_result>`, and loops until the model returns a plain-text answer. Edits and
commands ask for confirmation unless auto-accept is on.

### Making weaker models behave

Not every model follows the protocol perfectly, so Nocturne is defensive about it:

- **`edit_file` is whitespace-tolerant** — it matches exactly first, then falls back to a
  line-based match ignoring trailing whitespace and indentation (re-indenting your replacement
  to the file), so a near-miss `old_string` still lands. It never silently no-ops: results are
  clearly `EDIT APPLIED:` or `EDIT FAILED:`, and a failure shows the closest matching text.
- **Tolerant tool-call parsing** — recovers from the malformed JSON weaker models emit (extra
  braces, trailing junk, raw newlines inside strings) and the function-call-style `<tool>name(…)`
  variant, and de-duplicates repeated calls.
- **A short few-shot demo** is prepended to every request and the system prompt forbids the
  "I'll do it… Done" no-op, which stops weaker models refusing or claiming success without acting.
- **Transient upstream errors (502/503/504) are retried** automatically.

Reliability still varies by model — capable ones (e.g. `navy:claude-haiku-4.5`,
`navy:deepseek-v4-pro`, `gpt-5.5`) edit dependably; smaller/erratic ones less so. Switch any
time with `/model`. (Some models are also hosted on third-party providers that occasionally
return 502 — that's upstream, not Nocturne.)

Set `NOCTURNE_DEBUG=/path/to/log` to dump raw requests and responses for troubleshooting.

## License

MIT — see [LICENSE](LICENSE).
