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
with `artifactURL`, and `archived`. The ledger keeps the last 32 waves.

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

## Documentation

**SPECS.md** is the full technical contract: configuration, the MCP surface,
the `--tool-info` grammar, the queue/job rules and the scheduler↔sh boundary.
The code is the source of truth.

## License

AGPL-3.0-or-later — see [LICENSE](LICENSE).
