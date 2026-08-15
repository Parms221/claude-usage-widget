<div align="center">

# Claude Usage Widget

**A native Windows 11 taskbar widget for your Claude plan usage — the 5-hour
session window and weekly limit, live, right where your eyes already are.**

[Español](README.es.md)

[![CI](https://github.com/Parms221/claude-usage-widget/actions/workflows/ci.yml/badge.svg)](https://github.com/Parms221/claude-usage-widget/actions/workflows/ci.yml)
[![Security](https://github.com/Parms221/claude-usage-widget/actions/workflows/security.yml/badge.svg)](https://github.com/Parms221/claude-usage-widget/actions/workflows/security.yml)
![Windows 11](https://img.shields.io/badge/Windows%2011-widget-0078D4?logo=windows11&logoColor=white)
![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)
![No CGo](https://img.shields.io/badge/CGo-none-success)
![License](https://img.shields.io/badge/license-MIT-green)

<br>

<img src="docs/panel-dark.png" width="356" alt="Panel, dark theme"> <img src="docs/panel-light.png" width="356" alt="Panel, light theme">

A pill that lives *inside* the taskbar…

<img src="docs/pill-dark.png" width="300" alt="Taskbar pill">

…and expands into a Fluent-style flyout.

</div>

---

## Features

- **Taskbar pill** — Claude spark, session percentage, twin mini-bars
  (weekly / session) and a countdown until the 5-hour window frees up.
  It is a real *child window of the taskbar*, not a floating overlay: it
  never fights the shell over z-order, and it disappears together with the
  bar when a game or video goes full screen.
- **Flyout panel** — weekly limit with progress and reset time, local token
  count with estimated cap, 5-hour window card, **per-model breakdown**,
  last-7-days activity chart with usage streak, and a live status footer.
- **Native Windows 11 look** — acrylic backdrop, rounded corners, fly-in/out
  animations, hover states, and light/dark theme (follows Windows or set
  manually). The pill samples the actual taskbar pixels so it blends in
  under any accent color.
- **Official numbers** — utilization comes from the same OAuth usage endpoint
  that Claude Code's `/usage` screen reads. Per-model and daily stats are
  aggregated from your local Claude Code transcripts (incremental, deduped).
- **Respectful token handling** — an expired access token is refreshed by
  invoking the official CLI (`claude -p .`), never by calling the OAuth
  refresh endpoint directly, so your Claude Code session is never invalidated.
- **Reliable autostart** — "Start with Windows" registers a per-user
  Scheduled Task with a 10-second logon delay (the `Run` registry key is
  silently skipped by SmartScreen for unsigned executables).
- Single ~7 MB executable. Pure Go, **zero CGo**, no frameworks; the only
  runtime dependency is WebView2, which ships with Windows 11.

> The UI is currently Spanish-only. PRs adding localization are welcome.

## How it works

| Data | Source |
| --- | --- |
| Session (5 h) & weekly utilization, reset times | `GET https://api.anthropic.com/api/oauth/usage`, authenticated with your local Claude Code OAuth token |
| OAuth token, plan tier (`Max · 5x`, …) | `%USERPROFILE%\.claude\.credentials.json` (written by Claude Code) |
| Per-model tokens, daily bars, streak | `%USERPROFILE%\.claude\projects\**\*.jsonl` transcripts, deduplicated by `message.id + requestId`, cached per file |

The endpoint is polled once per minute (configurable) with automatic backoff
on rate limiting. Everything runs locally; nothing is sent anywhere except
the usage request to Anthropic's API.

## Requirements

- Windows 11 (WebView2 runtime included).
- [Go](https://go.dev/dl/) ≥ 1.22 to build.
- A logged-in [Claude Code](https://claude.com/claude-code) installation
  (`claude login`) — Pro, Max, Team or Enterprise plan.

## Build & run

```powershell
git clone https://github.com/Parms221/claude-usage-widget.git
cd claude-usage-widget
.\build.ps1          # go vet + go build (windowed, stripped)
.\ClaudeUsageWidget.exe
```

`ClaudeUsageWidget.exe -debug` enables WebView2 DevTools and verbose logging.

## Usage

- **Click the pill** to open the panel; click anywhere else, press `Esc`, or
  click the pill again to close it.
- **⋯ menu** — refresh now, theme (Auto / Dark / Light), start with Windows,
  quit.
- **"Ver detalles"** opens claude.ai's usage settings.

## Configuration

`%APPDATA%\ClaudeUsageWidget\config.json` (created on first run):

| Field | Default | Description |
| --- | --- | --- |
| `theme` | `auto` | `auto` follows Windows; `dark` / `light` pin it |
| `overlayTaskbar` | `true` | Embed the pill in the taskbar; `false` floats it above the work area |
| `marginX` | `16` | Left offset in logical pixels |
| `pollSeconds` | `60` | Usage endpoint poll interval |
| `historyDays` | `15` | Transcript scan window (bounds the streak counter) |
| `autoRefreshCli` | `true` | Allow `claude -p .` to renew an expired token |
| `acrylic` | `true` | Native Win11 blur behind the panel |
| `planLabel` | — | Override the plan caption in the header |

Log file: `%APPDATA%\ClaudeUsageWidget\widget.log`.

## Architecture

```
main.go                  wiring: single instance, DPI, poller, view-model
internal/winutil         pure Win32: frameless creation (CBT hook), DWM corners
                         & acrylic, taskbar parenting, input overlay, subclassing,
                         scheduled task, OS theme, screen-pixel sampling
internal/auth            Claude Code credentials + CLI-driven token refresh
internal/api             OAuth usage endpoint client
internal/history         JSONL transcript aggregation (per model / day / streak)
internal/vm              view-model builder and formatting
internal/ui              two WebView2 windows (pill + panel), bindings, geometry
internal/ui/assets       widget.html — the whole UI, dark + light
```

Two windows render the same embedded HTML in different modes:

- The **pill** is parented into `Shell_TrayWnd` (`SetParent`), so full-screen
  detection, z-order and taskbar-hiding come for free. Windows 11's XAML
  taskbar swallows WebView2's composition-pipeline input, so a tiny
  invisible classic Win32 overlay (layered, alpha 1) sits over the pill and
  relays mouse events to the page — fully event-driven, no polling, no
  global hooks.
- The **panel** is a regular top-level flyout: topmost while open, closes on
  focus loss or `Esc`, hides with `SW_HIDE`. It also runs the message loop
  and receives `TaskbarCreated`, recreating the pill if Explorer restarts.

More implementation notes (the fun ones):

- The window library shows a framed window before it can be styled; a
  thread-local **CBT hook** rewrites the `CREATESTRUCT` so windows are born
  frameless and off-screen — zero startup flash.
- The pill's flat look is achieved by **sampling the real taskbar pixels**
  (`GetPixel`) next to it and using that exact color as background
  (measured delta: 1/255 per channel).
- WebView2's transparent background is enabled through
  `ICoreWebView2Controller2::PutDefaultBackgroundColor`, reached via
  reflection into the library's unexported controller.
- The config loader tolerates UTF-8 BOMs, PowerShell 5.1's favorite gift.

## Troubleshooting

| Symptom | Where to look |
| --- | --- |
| Pill shows `!` or "Inicia sesión" | Run `claude login`; the widget reads Claude Code's credentials |
| "Límite de consultas · reintentando" | Endpoint rate limit (429); the widget backs off 5 min and recovers alone |
| Widget didn't start at logon | Task Scheduler → `ClaudeUsageWidget` shows the last run result |
| Anything else | `%APPDATA%\ClaudeUsageWidget\widget.log`, or run with `-debug` |

## Credits

- Monitoring approach inspired by
  [Claude-Code-Usage-Monitor](https://github.com/CodeZeno/Claude-Code-Usage-Monitor).
- WebView2 bindings: [jchv/go-webview2](https://github.com/jchv/go-webview2).
- UI designed with [Claude Design](https://claude.ai); spark path from
  [Simple Icons](https://simpleicons.org/).

## Disclaimer

This is an **unofficial**, community-built tool. It is not affiliated with,
endorsed by, or sponsored by Anthropic. "Claude" and the Claude spark logo
are trademarks of Anthropic, PBC, used here only to identify the service the
widget reports on.

## License

[MIT](LICENSE)
