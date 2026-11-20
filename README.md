# nerv-studio

**Русский вариант: [README.ru.md](README.ru.md)**

**nerv** is an omni-channel AI studio. A human is the **client** who issues a
commission; a **consumer AI** is the executor that runs a whole studio and
delivers a finished product. `nerv-core` is the thin Go daemon in the middle:
it orchestrates system-installed generation tools, runs a GPU scheduler and
serves one streamable MCP endpoint. Scope: a personal workstation up to a
small studio — a trusted perimeter, no auth, everything local.

This repository holds the core: the Go daemon and the technical contract
(SPECS.md) with a sample configuration. The generation toolbox is operator
property and is not shipped: any executable `sh` honouring the `--tool-info`
contract below plugs in. The web client (`nerv-gui`: AI chat with sub-agents
+ a non-linear composition table, a pure MCP client) has not started yet.

## Design laws

- **Minimum code, maximum system tools.** The core hardcodes nothing: every
  generator is a pluggable `sh` script discovered at runtime through
  `--tool-info`. Wrapping `sd-cli`, `ffmpeg`, `comfy-cli` — anything — is a
  script, not a patch.
- **Storage is pure configuration.** The `domains` map in nerv.json mounts
  arbitrary directories as MCP tools (`{path, commands, hint}`). Two commands
  are structural grants: `mount` marks *the playground* — the single io root,
  the queue-file home, the `/artifacts` HTTP mount — and `execute` marks a
  *toolbox*, where a queue's tool names resolve (no cross-toolbox fallback).
  There are no built-in roots.
- **MCP is the only surface.** HTTP-streamable JSON-RPC at `/mcp`, file
  serving at `/artifacts/<rel>`. `tools/list` is generated from the config:
  one tool per domain, then the utilities (`dice.roll`, `calc.resolution`).
  No private channels — any MCP client can drive the studio.
- **Data flows through files.** A job names its `io:"out"` output file and
  starts when every `io:"in"` input exists on disk. A queue is a
  file-existence dataflow — no graph, no declared edges.
- **The daemon owns the waves.** A submitted queue runs to the end inside the
  daemon (async waves survive disconnects), its result stays lookable by wave
  id, and an old same-name output is archived aside as `name-1.ext`.

## Quick start

Requirements: Go (1.26+), POSIX `sh`; a GPU + generation CLIs only if you run
real generators.

```sh
make build                      # -> bin/nerv-core
cp nerv.json.example nerv.json  # point the domains at your machine
./bin/nerv-core                 # 127.0.0.1:9696  /mcp + /artifacts
curl -s localhost:9696/mcp -X POST -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

The daemon derives the project root from its own location (the parent of
`bin/`), so relative domain paths survive any launch cwd. The minimal
`nerv.json` needs exactly one `mount` domain; a toolbox (your own directory
of sh scripts) is an `execute` grant:

```json
{
  "domains": {
    "tools":       { "path": "tools",        "commands": ["probe", "read", "execute"] },
    "artifactory": { "path": "/storage/out", "commands": ["browse", "probe", "read", "fetch", "write", "mount"] }
  }
}
```
## Configuration

Single file, `<project-root>/nerv.json` (or `--config <path>`). Precedence:
**explicit CLI flag > config file > built-in default**. The CLI accepts both
spellings (`--ai_url` and `--ai-url`); `--env KEY=VALUE` is repeatable and
merges over the `env` section. `debug` is flag-only (`--debug`): wave logs
run in the temp dir and fold into the queue file's `wave` result block.
Strict JSON: only the exact `_comment` key is ignored (documentation keys
`_comment_*` warn once and are dropped).

| key | default | role |
|---|---|---|
| `domains` | — (required) | the whole storage surface; see the next section |
| `ai_url` | `http://127.0.0.1:5000/v1` | OpenAI-compatible endpoint used for the VRAM wash-out (your local `llama-server`) |
| `llm_offload` | `false` | the VRAM XOR: `true` = every loaded chat LLM is unloaded before a wave with GPU jobs, and such waves run blocking; `false` = all waves async |
| `gpu_backend` | `""` (all detected) | pool filter, `backend[@host][:i or a-b]` terms, e.g. `cuda,rocm:0-1` |
| `external_gpu` | `""` | anonymous remote slots, `backend@host[:N]`, no detection, no health-check |
| `port` | `9696` | the single HTTP surface (`/mcp` + `/artifacts`) |
| `max_output_chars` | `32768` | volume guard for tool-result text; the full dump is re-served with `volume_ack=true` |
| `env` | `{}` | machine environment mixed **as-is** into every tool launch (caches, thread limits, model dirs); `NERV_*` keys are rejected — that prefix belongs to the scheduler↔sh contract |
| `media` | built-ins | budgets for bounded content into the consumer context (read/fetch): text caps, image/audio/video preview shape |

On a single-GPU workstation `llm_offload: true` is the switch that makes
generation and chat share one card: the chat model leaves VRAM for the wave's
duration and the wave holds the request (SSE progress when the client carries
a progressToken). This is a *cognitive* setting as much as a hardware one —
it decides whether the AI can watch its own generation happen.

## Domains: the storage surface

Everything the consumer AI can see on disk is a domain entry:

```json
"<name>": { "path": "<dir>", "commands": ["read", ...], "hint": "<one-line description>" }
```

- **`name`** *is* the MCP tool name (`[A-Za-z0-9_.-]`, reserved names aside) —
  the word the model types and reads all day. Naming is vocabulary design.
- **`path`** — absolute, or relative to the project root (never the launch
  cwd). The `mount` domain's directory is created on startup.
- **`commands`** — the allow-list the tool answers; a forbidden command
  returns `isError` with the available list. `fullRawAccess: true` ignores
  the allow-list entirely (escape hatch, not a norm).
- **`hint`** — lands verbatim in `tools/list`; for a language model this is
  not decoration but *the first line of instruction* for the domain.

The command set:

| command | grants |
|---|---|
| `browse` | directory listing (one level, or bounded recursive) |
| `probe` | metadata: file stats, media dimensions, safetensors headers; in an `execute` toolbox — the `--tool-info` dialect |
| `read` | bounded text/image reading with line/byte windows |
| `fetch` | take a file's bytes (artifact) into the workspace |
| `write` | create / overwrite / append text files (directories by trailing `/`) |
| `mount` | **structural:** this is *the* playground — the io jail for jobs, the queue-file home, `/artifacts` HTTP serving, wave lineage. Exactly one domain |
| `execute` | **structural:** this is a *toolbox* — queue `tool` names resolve to `<path>/<tool>.sh`, waves launch from here. Zero, one or many |

## Tuning the environment to a cognitive paradigm

What the consumer AI believes it *is* and *can do* in your studio equals the
set of tools it sees, multiplied by their descriptions. nerv exposes three
dials, all pure configuration:

1. **Capabilities — the command allow-list.** Grants are your permission
   model: a read-only library domain (`read`, `probe`) cannot be scribbled
   in; a scratch domain (`write`) without `browse` is a mailbox, not a
   cabinet. Trim grants to the role you are assigning, not to the filesystem.
2. **Behavior — the hint.** A domain hint is cheapest prompt-engineering
   that exists: "read README.md first; each doc names the next step and the
   file to write" turns a directory of notes into a *procedure* the AI
   walks. Discipline conventions live here too: "one chat = one project =
   one directory; create `<project>/` first" — the mount's hint carries it.
3. **World shape — which domains exist and what they are called.** Domain
   names become the model's nouns. A studio that separates `kb` (guides),
   `plots` (writing workspace) and `artifactory` (products) teaches the
   model that knowing, drafting and producing are different acts.

Some worked paradigms (all of them are just nerv.json):

- **Minimal generation bench** — `tools` (execute) + one mounted playground.
  The AI is a hands-on operator: probe a tool, write a queue, fetch a result.
- **Guided studio (kb walk).** Add `kb` with `read`+`probe` and a README.md
  entry point; the guide chain (each document naming its next step and the
  file to write) makes multi-phase workflows — brief → script → shots →
  assembly — drive the AI from disk instead of from chat memory. Knowledge
  is a file tree, not a system prompt.
- **Writing desk.** `plots` with `read`+`write`+`browse` and no toolbox at
  all: a pure-text studio for a writing agent (world bibles, characters,
  drafts). No GPU, no waves — the daemon is still the AI's whole filesystem
  API.
- **Library vs workbench.** Mount the same corpus twice with different
  grants: a weight store as `browse,probe,fetch` (read from outside by the
  sh scripts; the AI inspects it but never edits it) beside a writable
  working copy for experiments.
- **Multi-toolbox.** Several `execute` domains (`image`, `video`, `media`)
  split the tool surface into explicit rooms: queue names resolve inside one
  toolbox, no cross-toolbox fallback — the AI chooses a room, not a global
  namespace with 40 tools in it.
- **Heavy pipelines as queues.** Because a wave is a file-existence
  dataflow inside the daemon, a cognitive style of *plan on disk, run async,
  look up later* is available with zero client code: write the queue, fire
  `execute`, keep thinking; poll
  `{"command":"execute","wave":"latest"}` or `POST /wave/stop` (the red
  button) when needed.

Rules of thumb: use **grants** for safety posture, **hints** for culture,
**names** for vocabulary, and per-tool `hidden` fields (with defaults) to
keep knobs out of the model's face. The config is the studio's score; the AI
is playing it.

## Running a wave

A queue is a bare JSON array of `{tool, params}` lines. Write it into the
playground and launch it through the toolbox:

```json
[ { "tool": "krea2", "params":
    { "prompt": "a lighthouse at dawn, low tide, 35 mm photograph",
      "width": 1888, "height": 1056,
      "output": "lighthouse/portrait.png" } } ]
```

`tools { "command": "execute", "queue": "lighthouse/queue.json" }` admits the
lines (a bad line lands in `skipped`, never stops the wave) and picks the
mode:

- **async** (default) — the answer returns immediately with the wave id; the
  wave belongs to the daemon, a client disconnect cannot kill it.
- **blocking** — with `llm_offload: true` and any line asking for a GPU: the
  chat LLM is washed out of VRAM first, the request holds until the wave
  ends (SSE `notifications/progress` when the client carries a
  progressToken).

`{ "command": "execute", "wave": "latest" }` (or a wave id) answers a live
snapshot or the final result: jobs with `status` / `dur` / `slots`, `outputs`
with `artifactURL`, and `archived`. With `--debug`, the wave log folds into
the queue file itself as `wave.debug` — the queue file becomes the complete
record of its own run. The ledger keeps the last 32 waves.

## Writing your own tool

Drop an executable `.sh` into an `execute`-granted toolbox directory. The
contract:

- `--tool-info` answers a JSON interface (`summary` + `fields`); `io: "in"` /
  `io: "out"` mark file params, `hidden: true` (with a `default`) hides a
  knob from the consumer;
- params arrive as `NERV_<FIELD>` env vars; `io` file paths are **full
  absolute in both directions** — the mark is the contract, not the name;
- `gpu-require` / `gpu-backend` / `gpu-pin` are consumed by the scheduler and
  never injected (the device truth is `CUDA_VISIBLE_DEVICES` & co.);
- exit 0 plus all declared output files present means done; the core creates
  the output's parent directory and archives a same-name old file before the
  script starts.

See `tools/anima-aestetic.sh` for a complete worked example.

## Documentation

**SPECS.md** is the full technical contract: configuration, the MCP surface,
the `--tool-info` grammar, the queue/job rules and the scheduler↔sh boundary.
The code is the source of truth.

## License

AGPL-3.0-or-later — see [LICENSE](LICENSE).
