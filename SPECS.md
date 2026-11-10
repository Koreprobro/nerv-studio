# SPECS.md — nerv technical contract

The source of truth for the runtime contracts of nerv-core: file structure,
configuration, the MCP surface, the tool self-declaration grammar, the
queue/job contract and the scheduler↔sh boundary. The code is authoritative;
this document describes it. Symbol references point into `src/nerv-core/`.
The project's current state — what is live, what has not started — is in
AGENTS.md.

---

## 1. File structure

### 1.1 Project root

The daemon derives the project root from its own location:
`dirname(dirname(binary))` — the binary lives at `<root>/bin/nerv-core`
(`config.ProjectRoot`). The launch cwd never anchors anything; the systemd
`WorkingDirectory` is an operational anchor only.

### 1.2 The tree (development layout)

```
<project-root>/
├── bin/nerv-core          built daemon (gitignored; Makefile emits it here)
├── src/nerv-core/         Go sources (cmd/ + internal/)
├── src/nerv-gui/          web client (pure MCP client)
├── tools/                 consumer sh toolbox (*.sh, self-documenting)
├── kb/                    knowledge base (*.md, README.md is the entry)
├── docs/                  development material (not consumer kb)
├── nerv.json              runtime configuration (sample: nerv.json.example)
├── nerv-core.service      systemd user unit (symlinked from ~/.config/systemd/user/)
└── Makefile               build | test | vet | clean
```

### 1.3 Storage domains (config-driven, the flat-is-default law)

The storage surface is the `domains` map of nerv.json (§3.7): every entry is
a directory mounted as an MCP tool — the name is the tool name, the value is
`{path, commands, hint}`. There are **no built-in roots**: a daemon without a
configured `domains` map (or without exactly one `mount` among them) refuses
to start. The shipped studio uses four domains by author's convention:

| example domain  | example path     | purpose in the shipped studio                          |
|-----------------|------------------|--------------------------------------------------------|
| `tools`         | `tools`          | sh toolbox (unique-named `.sh`, `--tool-info` surface); grants `execute` |
| `kb`            | `kb`             | unique-named `.md` documents; `README.md` is the only pointer in the whole MCP surface |
| `artifactory`   | `/storage/out`   | artifact repository — the consumer-AI playground; grants `mount` |
| `loras`         | `/ai/loras`      | model/LoRA weights store                               |

There is no `logs` root: the wave log lives beside its queue file (§6.6),
written only with `--debug`.

Flat is the shipped default, **not a contract**: subdirectories are valid in
every domain (the browse mechanism supports trees; the listing bounds are
§3.3). `kb/` and `tools/` stay flat by naming discipline — unique names keep
the walk trivial for the consumer-AI.

### 1.4 Project discipline

One chat = one project = one playground (mount-domain) directory. The AI
creates `<project>/` first (`<playground> write path="<project>/"`) and keeps
its queues, outputs and notes inside it.

---

## 2. Configuration

### 2.1 Precedence

Built-in defaults < `nerv.json` < **explicitly passed** CLI flags
(unset flags never override the file — `fs.Visit`, `config/parse.go`).

- Config file default path: `<project-root>/nerv.json`; `--config <path>`
  must exist if given; a missing default-path file is fine.
- Malformed JSON or a wrong value type is fatal; unknown keys only warn
  (the typo trap for AI-authored files).
- CLI flag names accept both spellings: `--ai-url` normalizes to `--ai_url`
  (`normalizeArgs`). Bool flags need the `=value` form (`--llm-offload=true`).

### 2.2 Keys (storage lives in `domains` only)

| key                | default                       | meaning |
|--------------------|-------------------------------|---------|
| `domains`          | — (**required**)              | mounted directories: `path` + command allow-list + `hint` per domain (§3.7); no built-in roots |
| `ai_url`           | `http://127.0.0.1:5000/v1`    | OpenAI-compatible provider (VRAM wash-out) |
| `llm_offload`      | `false`                       | VRAM XOR + blocking GPU waves (§8) |
| `gpu_backend`      | `` (greedy scoop)             | pool selector `backend[@host][:i|a-b]` |
| `external_gpu`     | ``                            | remote slots `backend@host[:N]`, N default 1 |
| `port`             | `9696`                        | the single HTTP port (`/mcp` + `/artifacts`) |
| `max_output_chars` | `32768`                       | volume guard for tool-result text (§3.5) |
| `env`              | `{}`                          | machine env mixed into every launch, as-is (LoRA store: `LORA_MODEL_DIR`) |
| `media`            | built-in budgets (§3.3)       | read/fetch budget defaults |

`domains` is the only storage key. `debug` is **not a file key**: `--debug`
is a flag-only operator switch (§6.6). Domain `path` values may be absolute
or relative — relative paths anchor at the project root (§1.1), never at the
launch cwd. `env` keys starting with `NERV_` are **rejected at startup** with
a warning: the prefix belongs to the scheduler↔sh contract (§5).

### 2.3 Absent GPU is legal

A backend that cannot be detected only warns (`gpu.Build`); a CPU-only host
must still serve the studio. The pool is frozen at startup and never mutated.

---

## 3. MCP surface

### 3.1 Transport

Single HTTP server on `port`: `POST /mcp` (JSON-RPC 2.0, streamable HTTP —
SSE responses for long waves) and `GET /artifacts/<rel>` (the `mount` domain;
the `artifacts/` prefix folds out of any path the client hands back —
`job.NormalizePath`).

Methods: `initialize` (version negotiation against `ProtocolVersion =
2026-07-28`: the server answers with the client's version when the client
speaks at or below the server's), `ping`, `tools/list`, `tools/call`,
`notifications/*`.

### 3.2 The generated tool list

`tools/list` is **generated from the config** (§3.7): one tool per configured
domain, sorted by name, then the utilities. The toolbox scripts are *not*
listed (only prompt guides in `kb/` name tools). Domain descriptions come
from the domain `hint`; utilities carry built-in one-liners. The shipped
configuration yields:

1. `artifactory` — playground, the `mount` domain (§3.7)
2. `kb` — knowledge base (door: `README.md`)
3. `loras` — model store (probe sniffs safetensors headers)
4. `tools` — toolbox: `probe` (tool-info reader), `read` (script source),
   `execute` (the wave launch, §6)
5. `dice.roll` — utility (§7.1)
6. `calc.resolution` — utility (§7.2)

`gpu.list` (hidden operator diagnostic: pool slots + live load) is reachable
by name but never listed.

### 3.3 Domain commands

Every domain shares one browse mechanism (`internal/browse`); every path is
jail-checked (no absolute, no `..`):

| command   | semantics |
|-----------|-----------|
| `browse`  | one level; `recursive=true` gives a bounded tree (depth 3, 128/dir, 512 entries) |
| `probe`   | metadata only (size/mtime/mime/sniff; image WxH, ffprobe fields, safetensors header) |
| `read`    | bounded text window; `lines="A-B"` or `bytes="A-B"` (1-based, inclusive, sides optional, mutually exclusive), capped by `max_bytes` |
| `fetch`   | media into the block budget: images degrade (quality, then dimensions) via stdlib; audio/video via ffmpeg (thumbnail/clip); else raw passthrough |
| `write`   | text files only (extension allowlist), `content` + optional `append`; a trailing `/` means `mkdir` |

Per-domain **allow-lists** (the `commands` field) gate which commands the
consumer may call; a forbidden command answers `isError` with the available
list. A call with `fullRawAccess=true` ignores the allow-list entirely
(the GUI/operator escape hatch; hidden probe fields and raw-tagged waves
become visible too). Two entries of the allow-list are structural grants
rather than plain commands (§3.7): `mount` (a property, never callable) and
`execute` (callable: `{"command": "execute", "queue": …}` /
`{"command": "execute", "wave": …}`). On an `execute`-granted domain the
`probe` command — and a bare call with no command — speaks the tool-info
dialect instead of the file sniff.

Budgets (config `media`, overridable per call): text preview 16 KiB; media
block 512 KiB; image longest side 1600 px (video thumbnail 800 px); JPEG
quality 82; audio preview 16 kHz mono 64k; preview duration 30 s.

### 3.4 Visibility as configuration

The `domains` map is the whole cognitive-layer knob: the daemon
configuration, not the code, decides which tools exist, what the
consumer-AI can do with each of them (`commands`) and how each describes
itself (`hint`). Adding a directory to the surface is a config edit.

### 3.5 Volume guard

Any tool-result TEXT over `max_output_chars` is refused (`isError`) with a
head preview and the escape hatch revealed: re-send with
`volume_ack=true` for the full dump — a deliberate two-step dump, never an
accident. Media blocks carry their own budget (§3.3) and are not guarded.

### 3.6 Redaction

Server-absolute paths never reach the client: the redactor is built at
startup from the config — the project root becomes `<project>/`, every
domain root becomes `<domain-name>/` (longest root first, so a nested root
redacts to its own, deeper tag).

### 3.7 Generic storage domains

The storage surface is one generic `domains` map: directory name and path
are pure configuration, the MCP tool name is the domain name (validated:
MCP tool-name grammar `[A-Za-z0-9_.-]{1,64}`, no collision with the utility
names `dice.roll`/`calc.resolution`/`gpu.list`, non-empty path), and
`tools/list` is generated by iterating the map (sorted by name). There are
no default directories: a config without domains — or without exactly one
`mount` — is a startup error. A directory's purpose is its capability list —
plus exactly two **structural grants** the core needs beyond the consumer
surface:

- **`execute` — the domain's root is a toolbox.** A queue's `tool` name
  resolves to `sh <root>/<name>.sh` — the daemon composes the absolute
  path, the consumer never sees it (the script's own anchor stays
  `dirname "$0"`). The tool-info probe dialect (probe runs `--tool-info`,
  `minimal_capacity`, `hidden` redaction) follows the grant, and the wave
  launch is this domain's `{command: "execute"}` — one scheduler per
  toolbox, all sharing the frozen pool. Several execute-domains are legal:
  each is a toolbox with its own `execute`; a queue belongs to the toolbox
  it was submitted through.
- **`mount` — the domain's root is the playground.** The io path jail
  root (`io:"in"`/`io:"out"` absolutization), the queue-file location,
  the `/artifacts` HTTP mount, the `artifactURL` annotation, the wave-log
  location and the wave-answer lineage. Exactly one mount domain,
  validated at startup; the directory is created when missing.

```json
"domains": {
  "toolbox": { "path": "tools",          "commands": ["probe", "read", "execute"] },
  "out":     { "path": "/storage/out",   "commands": ["browse","probe","read","fetch","write","mount"] },
  "notes":   { "path": "/storage/notes", "commands": ["read", "probe"], "hint": "free-form notes" },
  "loras":   { "path": "/ai/loras",      "commands": ["browse","probe","fetch"] }
}
```

Invariants:

- **kb and loras dissolve** into ordinary named domains: the README-door
  is a convention carried by the `hint` text; the safetensors probe
  dialect is file-type sniffing and lives in the generic probe.
- **io stays single-rooted**: files of non-mount domains enter queues
  only as plain params or operator env (weights already do); there is no
  root discriminator in the lineage.
- **logs is not a root at all**: the wave log is a file beside its queue
  file (§6.6), written only with the `--debug` flag.
- The scheduler takes no configuration of its own: its toolbox directory
  and playground store are resolved from the map (the `execute`- and
  `mount`-domain roots) at startup; the launch contract (§5) does not
  change.

> Legacy config revisions that name separate root keys (`tools`,
> `artifactory`, `kb`, `lora_model_dir`, `logs`) or the `hints`/`debug`
> file keys read as unknown keys and warn once at startup; the old root
> flags (`--tools` &c.) are gone — the remaining flags are `--config`,
> `--debug` and one flag per runtime key of §2.2.

---

## 4. Tool self-declaration: the `--tool-info` grammar

### 4.1 Probe mechanics

`sh <toolbox>/<name>.sh --tool-info` runs in a **scratch dir with a minimal
env** (`PATH` only) — an unguarded script must not write into the daemon's
working directory. Stdout must be the JSON below; a non-zero exit or bad JSON
is the probe error. The tool name may omit the `.sh` suffix (resolution
tries it first).

### 4.2 Grammar

```json
{
  "summary": "one-liner for the consumer",
  "fields": [
    { "name": "prompt",       "description": "...", "required": true },
    { "name": "seed",         "default": -1 },
    { "name": "refs",         "io": "in",  "description": "input file(s), playground-relative" },
    { "name": "file-result",  "io": "out", "required": true, "description": "the artifact" },
    { "name": "gpu-require",  "default": 1 },
    { "name": "gpu-backend",  "default": "cuda" },
    { "name": "gpu-pin",      "default": "" },
    { "name": "internal-flag","default": true, "hidden": true }
  ]
}
```

Rules enforced by `job.ParseToolInfo`:

- every field has a non-empty unique `name`;
- `io` is `"in"`, `"out"`, or absent;
- a `hidden` field must declare a `default` (JSON `null` counts as absent);
- unknown attributes are tolerated (forward compatibility).

### 4.3 `hidden`

A **visibility marker, not an input filter**: hidden entries drop silently
from the probe view (no marker, no count — the consumer never learns anything
was hidden), but their values still flow to the script untouched, and a queue
may set them. `fullRawAccess=true` on `probe` shows everything.

### 4.4 Scheduler-relevant fields are ordinary fields

`gpu-require` (default `0` = CPU), `gpu-backend` (default `"any"`), `gpu-pin`
(default no pin) carry scheduler meaning by name convention only — no special
level in the envelope. A job param overrides the declared default per job.
`gpu-require` counts **pool slots, not physical cards** — an `external_gpu`
slot is an anonymous capability, never a card. The trio is consumed by the
scheduler at admission and never reaches the script env (§5).

### 4.5 Reserved names: conflict check

The `NERV_` prefix is the contract namespace. The core injects service vars
(pin envs and the calculator pair) beside the field vars; a declared field
whose name, normalized (`-` to `_`, uppercased — `job.NormEnvField`),
collides with a reserved service env name is rejected at probe time:
`ParseToolInfo` errors and `probe` / `execute` report it as a skip.

> The reserved set is `CALCULATED_WIDTH` / `CALCULATED_HEIGHT`
> (`job.ReservedEnvNames` — the single source of truth). User `env` keys
carrying the `NERV_` prefix are already rejected at startup (§2.2).

The set is small by design (§5.1 closes the env surface); the check is the
guard that keeps it that way.

---

## 5. Scheduler↔sh contract (job launch env)

Assembled per job by `scheduler.buildEnv`, in order:

1. `PATH` — explicit (the daemon env may be sparse under systemd).
2. **Pin env** for the captured LOCAL slots only, grouped per backend:
   `CUDA_VISIBLE_DEVICES=0,1` / `HIP_VISIBLE_DEVICES` /
   `ONEAPI_DEVICE_SELECTOR`. Unknown backends get no pin env (the script is
   the authority); remote slots get none (a local device selector would lie).
3. **User env** (config `env`) — as-is, no prefix; `NERV_*` keys cannot be
   there (startup rejects them; buildEnv guards again).
4. **Params** — one `NERV_<FIELD>` per effective param (sorted; field name
   uppercased, `-` to `_` via `job.EnvName`), except the scheduler-consumed
   trio (`gpu-require`, `gpu-backend`, `gpu-pin`): resolved at admission,
   never injected (the pin env is the device truth):
   - plain fields (no `io`): the value flows verbatim (stringified;
     arrays comma-joined, floats minimal-form, bools `true`/`false`);
   - `io:"in"` and `io:"out"`: the **FULL ABSOLUTE** path (arrays of paths
     comma-join absolute entries).
5. **Calculator pair** — when the tool declared both `aspect-ratio` and
   `target-mpx` (§7.3): `NERV_CALCULATED_WIDTH`/`NERV_CALCULATED_HEIGHT`.

The mark is the contract, not the name: the output field may be called
anything (`file-result`, `output`, …); `"output"` carries no privilege.

Execution: `sh <tool>` in its own process group; cancellation SIGKILLs the
whole group (generator children die with the wrapper); `WaitDelay` 10 s.
stdout+stderr stream into a capped 64 KB tail buffer (the failed-tail source)
and, with `--debug`, additionally into the wave log beside the queue file,
line-prefixed `<index> <tool> | ` (§6.6).

### 5.1 The closed env surface

The launch env is exactly `PATH` + pin envs + user `env` + `NERV_<FIELD>` +
`NERV_CALCULATED_*` — closed, with no directory-root exports:

- The toolbox anchor is `dirname "$0"` — the daemon launches
  `sh <abs>/<name>.sh`, and the toolbox uses this idiom for its own includes.
- Text assets live next to the script or arrive as `io:"in"` files — the kb
  is the chat's domain, not a tool's.
- The io mark is **symmetric**: file fields arrive absolute in both
  directions, so a script touches files only through its fields and never
  needs the playground root; scratch paths derive from the out field.
  Queue files and wave answers keep playground-relative paths —
  absoluteness is an env-transport concern only (lineage). Outputs
  additionally: the core has created the parent directory and archived an
  old same-name file before the script starts (§6.5), so the script writes
  the value directly.
- Machine paths live in the operator `env` and flow as-is (e.g.
  `LORA_MODEL_DIR=/ai/loras`, scripts read `${LORA_MODEL_DIR:-/ai/loras}`).
  Caveat: probes run with a `PATH`-only env, so probe output must never
  depend on the user `env` (it is static declaration anyway).
- The gpu trio is not injected — `gpu-require`/`gpu-backend`/`gpu-pin` are
  scheduler-consumed; the device truth for a script is the vendor pin env
  (`CUDA_VISIBLE_DEVICES` & co., which are not nerv names at all). The field
  names keep their scheduler meaning in `--tool-info`; they just do not
  occupy env space.

The closure is guarded: the `NERV_` prefix is reserved (user-env rejection,
§2.2, plus the conflict check, §4.5) and the computed pair rides inside it.

---

## 6. Queue / job contract

### 6.1 Submission

The launch command is `execute` on an `execute`-granted domain:
`{"command": "execute", "queue": "<playground-relative path>"}` — a **bare
JSON array**:

```json
[
  { "tool": "krea2",      "params": { "prompt": "...", "width": 1536, "height": 864,
                                      "file-result": "proj/shot1.png" } },
  { "tool": "media-trim", "params": { "input": "proj/shot1.png", "output": "proj/crop.png" } }
]
```

No top-level envelope: the file must begin with `[` — the daemon checks the
first non-space byte and rejects an object envelope by name. The daemon
reads the file, never mutates it. `{"command": "execute",
"wave": "latest"|<id>}` looks a wave up in the ledger (the last 32 waves);
the lookup answers through any toolbox — the ledger is daemon-wide.

### 6.2 Validation (lenient, submit-time)

Per entry (`Job.Check` against the probed interface):

- `tool` required; must resolve under the **submitting toolbox root** — the
  domain whose `execute` carried the queue (`.sh` may be omitted). There is
  no cross-toolbox fallback: a tool that lives only in another toolbox is
  skipped by name;
- every param key must be a **declared** field (public or hidden) — a typo
  cannot leak into the script as an invented env var;
- fields with a declared default not set by the job receive the default;
- `io` paths normalize (`artifacts/` fold, clean) and reject absolute or
  `..` paths; `io:"out"` requires a single non-empty path (absent value and
  no default means skip); `io:"in"` accepts a path or an array of paths;
- failure is **soft**: the entry lands in
  `skipped[{index, tool, reason}]` and the wave goes on; when nothing
  survives admission no wave starts and the skip list IS the answer.

### 6.3 Dataflow: files are the graph

No dependency graph, no cycle detection. A job's `io:"in"` paths are its
dependencies (the scheduler starts it when all of them exist on disk), its
`io:"out"` paths its products. When nothing can run and jobs still wait on
files that will never appear, they fail as **stuck** — a submit-side cycle
lands in exactly that trap.

### 6.4 Lifecycle

`pending -> running -> done | failed | stuck`.

- exit code = job status: non-zero means `failed` (with the tail of the log);
- **exit 0 without the declared output file(s) means `failed`** — the exit
  code alone is not trusted to mean a file landed;
- archive-before-start + output-dir creation happen at job start; the wave
  answer reports `archived[]` (the `name-<i>.ext` moves).

### 6.5 Overwrite protection (the core's law)

Before a job starts, an existing file the job is about to produce is moved
aside to `name-<i>.ext` (index before the extension, counted from 1, first
free index wins; in-place jobs — an output that is also an input of the same
job — keep the tool's own overwrite). The consumer-AI always gets exactly
the output name it declared; conflicts resolve in favor of the new file.
The same mechanism rotates old wave logs (§6.6).

### 6.6 Wave execution model

- A daemon-wide runner goroutine executes waves **FIFO, one wave at a
  time**; inside a wave, ready jobs pack into free slots concurrently
  (biggest `gpu-require` first to reduce fragmentation; slots captured
  exclusively).
- CPU jobs (`gpu-require: 0`) are unlimited — they need no slot.
- **Modes**: with `llm_offload=true`, a queue containing any `gpu-require > 0`
  line runs **blocking** — the request waits; SSE `notifications/progress`
  when the client carries a `progressToken`, plain JSON otherwise. Everything
  else runs **async** — the wave belongs to the daemon (a disconnect cannot
  kill it) and the answer returns immediately with the wave id.
- Wave id: wall-clock stamp + daemon counter (`20261110-031222-01`) — unique
  across restarts without a ledger on disk.
- **Wave log (`--debug`)**: one file beside the queue file,
  `<queue-stem>.log` (playground-relative), holding the jsonl event lines
  plus every job's streamed output (line-prefixed `<index> <tool> | `);
  an old same-name log rotates out through the archive mechanism (§6.5)
  before the wave opens its own. The wave answer carries the `log` pointer.
- Graceful shutdown: no new submits; queued and running waves finish within
  grace; stragglers get their process groups SIGKILLed.

### 6.7 GPU admission

Typed pool, frozen at startup (§2.3): greedy scoop or `gpu_backend` selector
`backend[@host][:i|a-b]`, plus anonymous `external_gpu` slots
`backend@host[:N]` (no detection, no health-check — transport is the script's
concern). VRAM is never accounted (memory is the sh author's business).
Admission resolves `backend ∩ pin` fail-fast — a nail that missed the wall
never enters the wave loop. `probe` on a toolbox answers `minimal_capacity`:
how many instances of the tool the pool seats at once (the planning number).

---

## 7. Utilities (outside the queue paradigm)

Two helpers live outside the tool/queue machinery — pure in-chat calls that
never touch the scheduler or the GPU (plus the hidden `gpu.list` diagnostic):

### 7.1 `dice.roll`

`{notation: "4d6kh3+2", seed?: 42}`. Grammar: `NdM` terms, `kh`/`kl` keep
bounds, `+/-` constants; bounds 96 notation dice / 16 terms / 1000 count /
1e6 sides. Entropy: an explicit `seed` wins (deterministic, replayable);
otherwise a physical draw — `/dev/hwrng` when present, else the kernel CSPRNG
(rejection sampling, never modulo bias). The choice is never reported.

### 7.2 `calc.resolution`

Aspect string in, generator-friendly `WxH` out. Accepts `"16:9"`, `"16x9"`,
`"16×9"` or a decimal factor (`"1.5"`). Studio spec defaults: sides multiple
of 32, ~2 MP target, area bounded 512²..1536²; overridable per call
(`multiple`, `targetMP`, `minSide`, `maxSide`). Answer:
`{aspect, width, height, pixels, multiple}`. The algorithm
(`internal/resolution`) lives here and nowhere else.

### 7.3 Calculator integration into the launch env

The consumer-AI plans the canvas and hands `aspect-ratio` + `target-mpx`
params to the tools; when a tool's `--tool-info` declares **both** fields
(normalized spelling — `aspect_ratio`/`target_mpx` count too), the scheduler
runs the same resolution algorithm at env-build time and injects the
computed sides as

```
NERV_CALCULATED_WIDTH=<w>
NERV_CALCULATED_HEIGHT=<h>
```

The script reads its canvas through the computed pair — the arithmetic is
done once in the core (`internal/resolution`, studio spec, `target-mpx`
overrides the area target). A broken aspect is the job's error
(`resolution calculator: …`), not a guess. The standalone `calc.resolution`
MCP tool stays for chat-time planning. Tools that take explicit `width`/
`height` params declare no pair and get no computed vars.

---

## 8. VRAM XOR (LLM offload)

With `llm_offload=true` the daemon talks to the OpenAI-compatible provider
(`ai_url`, default local llama-server): before any wave containing a
`gpu-require > 0` job it washes out the chat model —
`GET /v1/models` (status `loaded`) then `POST <root>/models/unload`
(llama.cpp exposes model management at the server root, not under `/v1`).
**Best-effort by contract**: errors are swallowed, the wave still runs.
Reload is the chat frontend's business, not the scheduler's. The XOR fires
on the wave itself: an in-chat tool call and the tools it launches share one
GPU, so offload-then-run is the only legal order (the call-site law).
