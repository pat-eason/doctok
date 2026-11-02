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

**Kling MCP backend (`engine/kling_mcp.go`).** `kling.backend = "mcp"` swaps in a second `KlingAPI` implementation (`Engine.newKlingBackend` dispatches; `NewKling` is still the API/test seam). The Kling MCP is OAuth-only, so each call is one headless `claude -p` (Haiku, `AllowMCP` scoped to the specific `mcp__<server>__<tool>`) that must return a small `--json-schema` result. `error_kind` maps onto `kling.APIError` codes (`no_credits`→1102, `auth`→1001 fatal; `busy`→1303 retryable; `not_found`→1203 triggers resubmit), so render's fatal/retry logic is shared. Spend rules: one generation call per attempt, never retried inside Claude, `imageCount` pinned to "1". Costs are credits, not units: `Estimate` zeroes Units/USD and sets `Backend`, and `recomputeKling` leaves `CostUSD` at 0. `MCPList` and relay runs must use the same `Dir` (`e.Home`), because project-scoped MCP servers are only loaded from their own directory. Tool schema and models came from live `who_am_i` (mcpVersion 1.3.3); re-verify if the MCP changes. The submit/poll path has only been exercised against fakes, because the real one spends credits.

**Presenter reference image (`engine/hero.go`).** Text-to-video clips can't keep one actor/set, so on the MCP backend (`NeedsHero`: mcp && !DryRun && !`kling.no_hero`) `Engine.Hero` first generates one 9:16 `text_to_image` (`kling-image-v3_0`) from the storyboard's cast + setting (`HeroPrompt`), downloads it to `HERO.jpg|png`, and leaves the generation `awaiting_approval`. `Render` returns `ErrHeroNotApproved` until `ApproveHero`. Clips then go through `image_to_video` with `CreateRequest.FirstImageURL` (the tool has no `aspect_ratio`; the first frame decides, which is why the hero is 9:16). The hero's task id is persisted at submit (resume polls, never re-pays), and `refreshHeroURL` re-reads the hosted URL at render start because hosted URLs may expire. The CLI gate is `heroGate` in `internal/cli/generate.go`; the web UI has no gate yet and will surface `ErrHeroNotApproved`. Also pinned: `prefer_multi_shots=false` for `kling-video-v3_0` (default true cuts inside a clip). The image-result shape (`image_url` from `query_tasks`) is relayed by Claude and has not been confirmed against a real finished image.

**Actor photo and directions (`engine/directions.go`, `hero.go`, `kling_mcp.go`).** `GenerateRequest.ActorImage` makes the user's photo the hero image instead of a `text_to_image`: `Create`/`checkActor` validates (MCP backend, not `no_hero`, not a chroma-key style, JPG/PNG, ≤30MB, ≥300px short edge after the 9:16 crop via `media.ActorCrop`) and snapshots it to `ACTOR.<ext>`; `heroFromPhoto` crops (`media.PrepareActor`, top-biased) to `HERO.jpg` and uploads via `klingMCP.UploadImage`: the `file_upload` tool returns a one-time ticket through the Claude relay, then **doctok POSTs the bytes itself** (multipart `ticket` + `file`; response `data.url`). The upload URL comes from the relay, so `allowUploadHost` pins it to https kling.ai/klingai.com before any photo leaves the machine. `Hero.Source` is `generated|upload`; uploads can't be "regenerated", and `refreshHeroURL` re-uploads the saved crop if the hosted URL stops resolving. With an actor, `KlingPromptWith` refers to "the person in the reference image" and swaps the style's `PromptSuffix` for a neutral one (styles bake in a set/lighting that fights the photo). `VoiceDirection`/`StageDirection` (≤400 chars) are inserted before the notes in the storyboard prompt (`withDirections`) and into every clip prompt (voice only when the clip has audio); they work on both backends.

**Video lengths (`engine/length.go`).** Supported range is `MinVideoSeconds`..`MaxVideoSeconds` (30–300). The storyboard window is per target (`TargetWindow`: ±15% with integer math, clamped to config), not the config's global min/max; `LengthGuidance` swaps the chapter/lore/recap structure by length (≤45s no chapters, ≤75s one, ≤130s two, else the full arc), and `DistillPromptFor` scales the distill step. Configs saved by older versions pin `180/300`; `fillDefaults` treats exactly that pair as uncustomized and lowers the minimum. `Engine.LengthOptions` is the single source for the CLI picker and the UI (`/api/meta` → `lengths`), each with its cost projection (API: price table; MCP: seconds × `kling.credits_per_second`, 0 when unset: credits are not published). Default model is `kling-3.0`; engine tests pin `kling-2.6` in `testEngine`.

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
