# doctok ✨

Turn large, gnarly technical docs into **3–5 minute TikTok-influencer explainer videos**:
vertical 1080×1920, jump cuts, dance breaks, burned-in word-by-word captions and
yassified Gen-Z / Gen-Alpha vernacular. It's giving documentation. No cap.

```
Notion page / Markdown ──► Claude Code (local)            ──► Kling (per-scene clips) ──► ffmpeg ──► VIDEO.mp4
                           distill → DISTILL.md                text-to-video, 9:16,        cuts, captions,
                           storyboard → SCRIPT.json            native audio                stitch, 1080×1920
```

## Install

Requires Go 1.26+, [Claude Code](https://claude.com/claude-code) (`claude` on your PATH) and
ffmpeg (`brew install ffmpeg`, no special build flags needed).

```sh
make build            # → bin/doctok
make install          # → $GOPATH/bin/doctok
```

## Quick start

```sh
doctok doctor                         # check Claude, Kling, Notion, ffmpeg
doctok configure                      # interactive setup (Kling key, defaults, …)
doctok generate ./docs/ARCHITECTURE.md --name arch-tea
doctok generate https://www.notion.so/acme/Payments-RFC-1a2b… --style pov-skit
doctok generate ./docs/api.md --dry-run   # storyboard + placeholder video, zero Kling spend
doctok ui                             # local web studio at http://localhost:7878
```

Running `doctok` with no arguments opens an interactive menu. Every command prompts for
whatever you leave out when it's attached to a terminal. Pass flags for scripted use; add
`--plain` to get log lines instead of the live TUI.

| Command | What it does |
|---|---|
| `doctor [--live] [--skip-mcp] [--json]` | Checks the Claude Code CLI, your **subscription login** (`claude auth status`), your **API key** (validated against the Anthropic API), which Claude instance will be used, **Kling** credentials and balance (`/account/costs`, free), model/resolution validity, the Notion token, the Notion MCP server in Claude Code, and ffmpeg. `--live` also sends a tiny prompt through Claude. |
| `configure` | Interactive setup. Secrets go to `~/.doctok/config.json` (mode 0600). |
| `generate [source] [--name] [--style] [--length 3m30s] [--dry-run] [--yes] [--music file] [--model] [--resolution] [--no-audio] [--no-captions] [--plain]` | Runs the whole pipeline. It pauses after the storyboard to show the exact Kling cost estimate and ask for approval (skip the prompt with `--yes`). |
| `resume [key]` | Continues an interrupted, failed or awaiting-approval generation. Finished stages, downloaded clips and **in-flight Kling tasks are reused**. |
| `list` · `show [key]` · `open [key]` · `rm [key]` | Inventory, details and metrics, play the video, delete. |
| `styles [-v]` | The style catalog. |
| `ui [--port] [--no-open]` | Local web UI (binds 127.0.0.1 only). |

## Artifacts & state

```
~/.doctok
├── config.json                 # settings (0600)
├── state.json                  # inventory + status + metrics of every generation
└── <generation-key>/
    ├── DOC.md                  # copy of the input document
    ├── DISTILL.md              # Claude's distillation
    ├── SCRIPT.json             # the storyboard (scenes, dialogue, cuts)
    ├── VIDEO.mp4               # the final 1080×1920 H.264/AAC video
    ├── clips/                  # raw Kling clips (kept so re-assembly is free)
    └── work/                   # caption PNGs, filter graphs, scratch
```

`state.json` records the following for each generation:
- status and stage, overall progress, and any errors
- start time, finish time, wall-clock duration and active duration, plus timing per stage
- every Claude call: purpose, model, cost, tokens, and billing mode (`subscription` or `api`)
- the Kling model, units per clip (marked as **reported** by Kling or **estimated** from the price table), the USD cost, task IDs and the retry count per clip
- the final video's duration, resolution and size

Set `DOCTOK_HOME` to use a different directory.

The CLI and the UI can run at the same time. State writes are serialized with a file lock.
If a process dies mid-run, the generation shows up as `interrupted`, and `doctok resume`
picks it up.

## Claude: subscription or API key

Claude Code (`claude -p`) does all the reading and writing: Notion fetching via MCP,
distillation, and the storyboard (constrained by a JSON schema). `claude.mode` picks the instance:

- `auto` (default): your Claude subscription if `claude auth status` shows you're logged in, otherwise the API key.
- `subscription`: `ANTHROPIC_API_KEY` is stripped from the subprocess so your Pro/Max plan is used. Reported costs are API-equivalent.
- `api`: runs with your configured key (`ANTHROPIC_API_KEY` or `doctok configure`) and is billed to your API account.

Content-generation runs pass `--tools ""` and `--strict-mcp-config`, so they're pure
text-in/text-out. `claude.model` (e.g. `opus`, `sonnet`) and `claude.max_budget_usd`
are optional.

## Notion

Pages can be fetched two ways (`notion.mode`):

1. **Notion API**: set `NOTION_TOKEN` (an [internal integration](https://www.notion.so/my-integrations)
   token) and share the page with the integration. The page and its nested blocks are
   converted to Markdown.
2. **Claude Code MCP**: if you have a Notion MCP server configured in Claude Code, doctok
   asks headless Claude to fetch the page through it. Only that server's tools are allowed,
   and the result must come back through a structured schema, so a failed fetch is detected
   instead of being saved as the document.
   ```sh
   claude mcp add --transport http notion https://mcp.notion.com/mcp   # then run `claude` → /mcp to sign in
   ```
   Note: claude.ai *connectors* (the "claude.ai Notion" entry in `claude mcp list`) are not
   loaded by headless `claude -p`. Use a locally added server. `doctor` warns about this.

`auto` uses the API when a token is set and falls back to MCP otherwise.

## Kling

Every storyboard scene becomes one Kling text-to-video job: 9:16, 1080p by default, with
native audio so the creator actually speaks the dialogue. Jobs run in parallel up to
`kling.concurrency`. Rate-limit and overload errors back off and retry. Failed clips are
re-submitted up to 3 times. Auth and balance errors (codes 1000–1004, 1100–1103) stop
the render immediately.

| Model | Clips | Audio | Units/s @1080p |
|---|---|---|---|
| `kling-2.6` (default) | 5 / 10 s | optional (needs 1080p) | 0.5 silent · 1.0 with audio |
| `kling-3.0` | 3–15 s | optional | 0.8 · 1.2 |
| `kling-3.0-turbo` | 3–15 s | always | 1.0 |
| `kling-2.5-turbo` | 5 / 10 s | none | 0.5 |

A 3:30 video on the default model is about 210 units, roughly $29 at $0.14 per unit.
`doctor` shows your balance. `generate` shows a rough budget before it starts and an exact
estimate before any credits are spent.

**API styles.** `kling.api_style` = `standard` (API key → `POST /text-to-video/<model>`,
`GET /tasks`, per-task billing) or `legacy` (access key + secret → HS256 JWT,
`POST /v1/videos/text2video`). `auto` picks `standard` when an API key is configured. The
base URL defaults to `https://api-singapore.klingai.com`.

> ⚠️ Kling's official docs only render client-side. This client was built against a
> mirrored copy of Kling's API reference and price table (snapshot 2026-09-20). If Kling
> changes a field name or price, adjust `engine/kling/` or `kling.unit_price_usd`. When
> Kling reports billing per task, doctok uses the reported units.

## Styles

Presets come from a scan of TikTok trends (Oct 2026). They're built on recurring formats,
not individual sounds, since most trends die within days. Each preset sets:
- the creator persona and setting
- the narrative arc
- camera grammar and a Kling prompt suffix
- the edit grammar
- a movement / dance vocabulary
- voice direction and a slang bank
- the caption look

| id | vibe |
|---|---|
| `bestie-explainer` 💅 | front-camera bestie spilling the tea about your docs (default) |
| `green-screen-yapper` 🟩 | commentator keyed over rendered excerpts of *your actual doc* |
| `pov-skit` 🎭 | POV sketches where your services are the characters |
| `brainrot-mascot` 🤖 | an obviously-synthetic toy mascot, maximum chaos |
| `reality-confessional` 🌹 | dating-villa confessionals about technical drama |
| `unc-vs-gen-alpha` 👴 | a Gen-Alpha teen explains it to their out-of-touch unc |
| `throwback-2016` 📼 | "2026 is the new 2016" washed-out six-second-era energy |
| `top5-hype` 🕺 | ranked countdown with a dance break per number |
| `cozymaxxing-study` 🕯️ | calm, soft-lit explainer for heavy topics |

What doctok does on every video:
- Uses Kling's `[Label, tone] says: "…"` dialogue format.
- Pastes a character block, identical byte for byte, into every clip so the creator stays consistent.
- Keeps dialogue to about 2.3 words per second.
- Adds "no background music" to every prompt, with an optional `--music` bed you have rights to, mixed in ffmpeg.
- Places captions in TikTok's UI-safe zones.
- Skips slang that's misogynistic or hateful, and avoids real people, brands and IP.
- Tags the output file as AI-generated. Label it as AI-generated when you post.

## Video assembly

ffmpeg handles every edit step. Captions are rendered to PNG in Go and composited with `overlay`, so a stock ffmpeg works (no libass or freetype needed). The steps:
1. Normalize each clip to 1080×1920 at 30 fps.
2. Apply the scene's cut: punch-in jump cut, white flash, or soft fade. Some styles alternate zoom between clips.
3. Apply the style's colour grade and chroma-key compositing.
4. Burn in word-chunk captions with the active word highlighted, plus a top "sticker" overlay.
5. Concatenate the clips losslessly and optionally mix in a music bed.

## Architecture

```
cmd/doctok/            main
internal/cli/          CLI frontend (cobra + huh forms + bubbletea live progress)
internal/ui/           local web UI frontend (net/http + SSE + embedded single page)
engine/                generation engine — no CLI/UI code; Create / Plan / Render / Run / Doctor + Events
  claude/              headless Claude Code runner (subscription vs API key, MCP scoping)
  kling/               Kling client (standard + legacy APIs, model/price registry)
  notion/              Notion API → Markdown
  media/               ffmpeg assembly, caption/doc-card rendering
```

The engine is a library. Frontends call `Create` → `Plan` (ingest, distill, storyboard;
stops at `awaiting_approval` with an estimate) → `Render` (Kling, then assemble), and
observe `Event`s that carry a full generation snapshot.

## Development

```sh
make test     # go test -race ./...
```

The tests include a full pipeline run against a fake `claude` binary and a fake Kling
server, with real ffmpeg. They cover retries, interrupt-and-resume reusing in-flight tasks,
fatal-balance handling, and dry runs.

## Environment variables

| Variable | Purpose |
|---|---|
| `DOCTOK_HOME` | artifact dir (default `~/.doctok`) |
| `KLING_API_KEY` / `KLING_ACCESS_KEY` + `KLING_SECRET_KEY` / `KLING_BASE_URL` | Kling credentials |
| `ANTHROPIC_API_KEY` (or `DOCTOK_ANTHROPIC_API_KEY`) | API-key billing for Claude |
| `NOTION_TOKEN` | Notion integration token |
| `DOCTOK_FONT` | caption font (TTF/OTF); default Arial Black, falling back to embedded Go Bold |
| `DOCTOK_FFMPEG` / `DOCTOK_FFPROBE` | ffmpeg binaries |

Environment values override `config.json` and are never written to it.
