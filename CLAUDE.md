# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
make build                      # → bin/doctok (version via -ldflags -X main.version)
make test                       # go test -race ./...
make vet / make fmt             # go vet ./... / gofmt -w .
go test ./engine -run TestPipelineEndToEnd -v     # single test
go test ./engine/kling -run TestStandardAPI -v
```

Engine tests shell out to real `ffmpeg`/`ffprobe` (skipped if missing) and to fake `claude` shell scripts written into `t.TempDir()`. Kling is faked with `httptest`. No network or credentials are needed.

Manual runs: set `DOCTOK_HOME=<scratch dir>` so you don't touch the real `~/.doctok`. Use `--dry-run` to exercise the whole pipeline without Kling credits. Dry runs still make real (cheap) Claude calls. `--plain` swaps the bubbletea TUI for log lines, which is needed when there's no TTY. `DOCTOK_NONINTERACTIVE=1` disables all prompts.

## Architecture

`engine/` is the generation engine. It's a library with no CLI or UI code. `internal/cli` (cobra + huh forms + bubbletea progress) and `internal/ui` (net/http + SSE + one embedded `static/index.html`) are separate frontends over its public API. Keep that boundary: frontends only call `engine.*`, and the engine never prints.

**Pipeline and lifecycle.** `Create` → `Plan` → `Render` (`Run` = Plan + Render).
- `Create` validates input, reserves a unique key and snapshots the style to `STYLE.json`.
- `Plan` runs the ingest → distill → script stages, then stops at status `awaiting_approval` with an `Estimate`.
- `Render` runs the render → assemble stages.

Every stage is skipped if `StageTimes[stage].FinishedAt` is set and its artifact exists. That makes `Plan` and `Render` idempotent, and they double as resume. Per-clip Kling `TaskID`s are persisted as soon as tasks are submitted, so a resumed render polls in-flight tasks instead of resubmitting (and paying twice). When the context is canceled, `fail()` marks the generation `interrupted` (resumable) instead of `failed`.

**State.** `engine/state.go` `Store` guards `~/.doctok/state.json` with an in-process mutex plus `flock`, so the CLI and UI can run at once. Every mutation goes through `Store.Update(key, fn)`, a read-modify-write under the lock. Never hold a `*Generation` across updates and write it back. A `running` generation whose `OwnerPID` is dead reads back as `interrupted`. Empty maps are dropped by `omitempty`, and `read()` re-initializes them.

**Events.** The engine emits `Event`s, each carrying a full `*Generation` snapshot. Frontends render from the snapshot, not from deltas. The UI's SSE hub only sees jobs started in its own process, so the page also polls `/api/generations` to pick up CLI-driven runs.

**Claude (`engine/claude`).** Everything runs through headless `claude -p --output-format json` with the prompt on stdin.
- Billing: `Runner.Env()` strips `ANTHROPIC_API_KEY` for subscription mode and injects the configured key for API mode. `Engine.resolveBilling` picks the mode once per engine.
- Structured output uses `--json-schema` and is read from `structured_output`.
- Tool scoping is per request:
  - default: `--tools "" --strict-mcp-config` (no tools)
  - `AllowMCP`: Notion fetch through a specific MCP server's tool prefix
  - `Tools`: web research for trends
- `OnTool` switches to `stream-json --verbose` and reports tool calls live.
- claude.ai connectors aren't loaded in headless mode. Only locally added MCP servers work.

**Kling (`engine/kling`).** Two API surfaces:
- `standard`: API key, `POST /text-to-video/<model>`, `GET /tasks?task_ids=`, billing reported per task.
- `legacy`: AK/SK HS256 JWT, `/v1/videos/text2video`.

`models.go` holds the model registry: allowed durations, audio support, and units/second. The registry drives the storyboard JSON schema (the `seconds` enum), `Normalize`, and cost estimates. The client was built against a mirrored doc snapshot (2026-09), so verify against live behavior when touching it. `APIError.Fatal()` (auth/balance) cancels the whole render. `Retryable()` errors back off.

**Storyboard → prompts (`engine/storyboard.go`).**
- Claude returns a `Storyboard`. `Normalize` snaps durations to the model's set and forces the runtime into [min, max] by resizing or dropping scenes.
- `KlingPrompt` pastes identical character blocks into every clip and uses Kling's `[Label, tone] … says: "…"` dialogue format.
- It always appends "no on-screen text": captions are added later by ffmpeg.

**Media (`engine/media`).** Uses only stock ffmpeg filters, because the local ffmpeg has no libass/freetype (no `subtitles`/`drawtext`).
- Captions, top stickers and green-screen doc cards are rendered to PNG in Go (`x/image`, Arial Black, falling back to embedded Go Bold) and composited with `overlay` plus `enable='between(t,…)'`.
- Each segment is normalized to identical codec params so `Concat` can use the concat demuxer with `-c copy`.
- Dry runs use `Placeholder` clips (green for chroma-key styles).

**Styles and trends.**
- `styles_catalog.go` is the built-in catalog (dated `BuiltinTrendsDate`).
- At runtime, use `e.Styles()` / `e.Style(id)`, which merge `~/.doctok/trends.json`, or `GenerationStyle(g)` for an existing generation (reads the `STYLE.json` snapshot). Use package-level `LookupStyle` only for built-ins.
- `doctok trends refresh` → `RefreshTrends` runs Claude with WebSearch/WebFetch. It refuses results produced without searches. Every preset passes through `SanitizeStyle`, which treats web-derived text as untrusted because it flows into prompts: it strips URLs, removes banned slang, clamps caption zones and enforces the prompt suffix.
- `ApplyTrends` archives the previous catalog to `trends-history/`.

**Config.** `~/.doctok/config.json` (0600). Environment variables (`KLING_*`, `ANTHROPIC_API_KEY`, `NOTION_TOKEN`) override file values and are tracked in `envOverrides`, so `SaveConfig` never persists them.
