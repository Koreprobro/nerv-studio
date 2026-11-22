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

There is no `logs` root: the `--debug` wave log runs in the system temp dir
and folds into its queue file's wave block (§6.6).

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
| `port`             | `9696`                        | the single HTTP port (`/mcp` + `/nest`) |
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
SSE responses for long waves), `POST /wave/stop` (the red button outside
MCP — §6.6) and `GET /nest/<domain>/<rel>` — the file-serving surface of
every storage domain **except** `execute`-granted toolboxes (a toolbox is a
capability, not a file: script sources never enter the consumer's context
by URL). A domain answer's `artifactURL` is exactly this URL. The
`nest/<active mount>/` prefix folds out of any path the client hands back
(`job.NormalizePath`) into a playground-relative io path; a path naming
another nestable domain stays a literal io path (it fails existence until
the file is copied into the playground).

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

### 3.3 Domain commands

Every domain shares one browse mechanism (`internal/browse`); every path is
jail-checked (no absolute, no `..`):

| command   | semantics |
|-----------|-----------|
| `browse`  | one level; `recursive=true` gives a bounded tree (depth 3, 128/dir, 512 entries) |
| `probe`   | metadata only (size/mtime/mime/sniff; image WxH, ffprobe fields, safetensors header) |
| `read`    | bounded text window; `lines="A-B"` or `bytes="A-B"` (1-based, inclusive, sides optional, mutually exclusive), capped by `max_bytes`; the text door: binary content is refused with a pointer to `fetch`/`probe` |
| `fetch`   | media into the block budget: images degrade (quality, then dimensions) via stdlib; audio/video via ffmpeg (thumbnail/clip); else raw passthrough; the media door: plain text is refused with a pointer to `read` |
| `write`   | text files only (extension allowlist), `content` + optional `append`; a trailing `/` means `mkdir` |
| `help`    | the always-available manual: no `path` lists the commands this domain answers to (one WHAT-line each), `path="<method>"` explains one method — its purpose, its parameters, the door boundaries; implementation details stay out of the consumer's creative context |

Per-domain **allow-lists** (the `commands` field) gate which commands the
consumer may call; a forbidden command answers `isError` with the available
list. `help` stands outside the allow-list — every domain answers it, and its
listing reflects the actual grants (on an `execute`-granted domain the `probe`
entry speaks the tool-info dialect and the `execute`/`check` pair is listed).
A call with `fullRawAccess=true` ignores the allow-list entirely
(the GUI/operator escape hatch; hidden probe fields and raw-tagged waves
become visible too). Two entries of the allow-list are structural grants
rather than plain commands (§3.7): `mount` (a property, never callable) and
`execute` (callable: `{"command": "execute", "queue": …}` /
`{"command": "execute", "wave": …}`), with `check` as its callable
companion (`{"command": "check", "queue": …}` — the dry run, §6.1). On an
`execute`-granted domain the
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
accident. The cap holds on **both transports**: JSON answers and the final
event of a streamed blocking wave alike (on the stream a finished wave is one
`execute wave="<id>"` away — a lookup reshoots nothing). Media blocks carry
their own budget (§3.3) and are not guarded.

### 3.6 Redaction

Server-absolute paths never reach the client: the redactor is built at
startup from the config — the project root becomes `<project>/`, every
domain root becomes `<domain-name>/` (longest root first, so a nested root
redacts to its own, deeper tag).

Redaction is enforced at the **response boundary**, not per error site:
every text block of every tool result passes through the redactor on both
transports (plain JSON, and the final event of a blocking SSE stream), and
progress notification lines pass through it too. Error texts, file
content (a stamped queue file may carry wave-debug tails quoting absolute
vendor paths; paths the job itself writes are relative by construction —
§5) and wave blocks are all mapped; base64 media blocks pass through
untouched.

The surface is the consumer-AI's **jail**: its world is relative paths
and `<name>` root tags, and a full server path survives only in debug
artifacts (the wave log file on disk). The system temp directory redacts
to `<tmp>/`; any other standalone absolute token (two or more segments)
collapses to `<path>/basename`. URLs — including the studio's own
`/nest/` links — are not server filesystem paths and pass through
untouched.

### 3.7 Generic storage domains

The storage surface is one generic `domains` map: directory name and path
are pure configuration, the MCP tool name is the domain name (validated:
MCP tool-name grammar `[A-Za-z0-9_.-]{1,64}`, no collision with the utility
names `dice.roll`/`calc.resolution`, non-empty path), and
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
  root (io fields arrive playground-relative; existence checks and the
  store's archive resolve against it), the **job cwd** at launch (§5), the
  queue-file location, the `/nest/<domain>` HTTP serving, the `artifactURL`
  annotation, the wave-log location and the wave-answer lineage. Exactly
  one mount domain, validated at startup; the directory is created when
  missing.

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
- **logs is not a root at all**: the `--debug` wave log is transient (system
  temp) and ends up folded into the queue file's wave block (§6.6).
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
tries it first). The probe is a declaration read, not a job: it stays out of
the playground so a bare `--tool-info` never touches the consumer's world or
depends on the launch env (§5.1).

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
   - `io:"in"` and `io:"out"`: the **playground-relative** path exactly as
     the consumer wrote it (arrays of paths comma-join entries). The job
     runs from the mount root, so the value is directly usable as a file
     operand (the script's working world is the consumer's world).
5. **Calculator pair** — when the tool declared both `aspect-ratio` and
   `target-mpx` (§7.3): `NERV_CALCULATED_WIDTH`/`NERV_CALCULATED_HEIGHT`.

The mark is the contract, not the name: the output field may be called
anything (`file-result`, `output`, …); `"output"` carries no privilege.

Execution: `sh <tool>` with **cwd set to the active mount domain's root**, in
its own process group; cancellation SIGKILLs the
whole group (generator children die with the wrapper); `WaitDelay` 10 s.
stdout+stderr stream into a capped 64 KB tail buffer (the failed-tail source)
and, with `--debug`, additionally into the wave log (system temp, §6.6),
line-prefixed `<index> <tool> | ` (§6.6).

### 5.1 The closed env surface

The launch env is exactly `PATH` + pin envs + user `env` + `NERV_<FIELD>` +
`NERV_CALCULATED_*` — closed, with no directory-root exports:

- The toolbox anchor is `dirname "$0"` — the daemon launches
  `sh <abs>/<name>.sh`, and the toolbox uses this idiom for its own includes.
- The **cwd is the playground**: jobs launch from the mount root, so the
  consumer's world is the script's working directory and `$PWD` is the
  playground anchor (`external_gpu` transports scp/rsync from `$PWD/rel`).
  A write to an unguarded relative path lands in the playground — the
  consumer's writable world — not in the daemon's tree.
- Text assets live next to the script or arrive as `io:"in"` files — the kb
  is the chat's domain, not a tool's.
- The io mark is **symmetric**: file fields arrive playground-relative in
  both directions — the same string the consumer wrote, the queue file
  holds and the wave answer echoes; absoluteness is an internal store
  concern (existence checks and the §6.5 archive resolve against the mount
  root). A script touches files only through its fields, relative to the
  cwd, and never needs the playground root by name; scratch paths derive
  from the out field. Outputs
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
`{"command": "execute", "queue": "<playground-relative path>"}`. A queue
file is a **JSON array of lines** or the **envelope** (the daemon reads the
first non-space byte to tell them apart):

```json
[
  { "tool": "krea2",      "params": { "prompt": "...", "width": 1536, "height": 864,
                                      "file-result": "proj/shot1.png" } },
  { "tool": "media-trim", "params": { "input": "proj/shot1.png", "output": "proj/crop.png" } }
]
```

```json
{ "defaults": { "tool": "krea2", "params": { "prompt": "...", "aspect-ratio": "16:9" } },
  "jobs":     [ { "params": { "file-result": "proj/shot1.png" } },
                { "params": { "file-result": "proj/shot2.png", "seed": 7 } } ],
  "wave":     { } }
```

`defaults` is a template line merged into every job: top-level fields
(`tool`) fill only the line's silences, `params` merge key-wise — the
line's value wins whole (no splice of maps or arrays). `wave` is the
daemon-written result block (below), ignored on read — a finished queue
file resubmits as is, a reshoot is safe. An object without a usable `jobs`
array is rejected by name before the wave starts.

**`_comment` is the file's own annotation:** a free-form string at the
envelope level or on any job line (a wave's purpose, a board-row citation).
The scheduler ignores it — typed read, and §6.2 guards `params` keys only —
and the write-back stamp preserves it, so the annotation survives every
fire. It belongs beside `tool`/`params`, never inside `params` (there the
undeclared key would skip the job).

**Scoped `defaults`.** Inside the jobs array (bare or enveloped) a line
carrying a top-level `"defaults"` key is a **directive, not a job**: its
body takes the envelope-defaults form, layers over the file's active
template — the later declaration wins per key, silence keeps the earlier
value — and governs the lines that follow, **within that file only** (a
multi-file set leaks no scope). The job line wins over every defaults.
Directive lines do not count as jobs and never reach the ledger.

**Batches live in the queue, not in the backend.** N job lines sharing one
defaults header are a batch; the scheduler fans them across the GPU pool,
which beats the backend's own batch dimension on one GPU. So tool scripts
pin the backend-native batch to 1 (a literal `batch_size: 1` in the
workflow) and `--tool-info` never carries a batch field.

**Variant lists — the dynamic prompt, minimal.** A string param may carry
`{{cat|dog}}` lists: one option is picked at launch and the backend receives
the resolved string only — the sigil never leaks. `{{0.7::cat|0.3::dog}}`
picks weighted (a missing weight means 1; an unparsable or negative prefix
stays option text). The sigil is guarded on two counts — double braces, and
a pipe on the top level inside — so `{{plain text}}`, single braces (a
layout-JSON prompt) and bbox brackets pass untouched; a backslash before the
opener (`\{{a|b}}`) escapes it. Picked options expand in turn, so lists
nest (`{{photo of {{cat|dog}}|sketch}}`, capped at 8 levels); the guard
reads each group's own top level — a group without a pipe of its own stays
literal whole, inner lists included. Options are raw text: whole phrases
roll, and spaces belong to the option (nothing is trimmed). The pick is seeded from the job's declared `seed` param when
present — a reshoot of the stamped file reproduces it byte for byte — and
from process entropy otherwise. The fire is lineage: the queue line keeps
the template, the sub-ledger records the resolved params per job (`resolved`).

The `queue` argument takes a **set**: one path or a comma-separated list
(`"proj/q-s1.json, proj/q-s2.json"`), each file holding either shape; all
files are read and merged in list order into **one wave** — the elephant is
eaten in parts and fired at once. A file that fails to resolve or parse
rejects the whole launch before any wave starts; the wave (and its debug
fold, §6.6) anchors at the first file. `{"command": "check", "queue": …}`
is the launch's **dry run**: it reads the set and rehearses admission
(§6.2), answering `{files: [{file, jobs}], jobs, admitted, skipped}` —
nothing launches, no wave opens, nothing moves on disk; a parse failure or a
zero-admission set answers `isError` (naming the file / listing the skips).
`{"command": "execute",
"wave": "latest"|<id>}` looks a wave up in the ledger (the last 32 waves);
the lookup answers through any toolbox — the ledger is daemon-wide.

**Wave result write-back.** The queue file is a self-describing ticket:
recipe in, result out. When the wave ends, the daemon atomically
(temp file + rename) stamps a `wave` block into every file of the set:
`{wave, mode, ok, started, finished, jobs}` where `jobs` is the file's own
sub-ledger on **local** indexes (tool, outputs, archived, artifactURL,
status, error, dur, plus `resolved` when variant lists fired); the anchor
file additionally carries `skipped` and,
with `--debug`, the `debug` tail (plus `debug_note` when truncated). A bare
array is promoted to the envelope by the stamp; the block never affects a
later read (reshoot). The stamp is best-effort — a file-write failure is
reported on stderr and the wave answer stands. A client that died mid-wave
(chat reload, agent disconnect) recovers from this block: after reconnect,
read the queue file instead of chasing the lost response.

### 6.2 Validation (lenient, submit-time)

Per entry (`Job.Check` against the probed interface):

- `tool` required; must resolve under the **submitting toolbox root** — the
  domain whose `execute` carried the queue (`.sh` may be omitted). There is
  no cross-toolbox fallback: a tool that lives only in another toolbox is
  skipped by name;
- every param key must be a **declared** field (public or hidden) — a typo
  cannot leak into the script as an invented env var;
- fields with a declared default not set by the job receive the default;
- `io` paths normalize (`nest/<active mount>/` fold, clean) and reject absolute or
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

**Only humans delete.** The consumer surface has no delete verb: the AI
writes, moves and renames, and the archive above is a *move aside*, never a
deletion. The playground only grows — pruning is a human privilege.

### 6.6 Wave execution model

- A daemon-wide runner goroutine executes waves **FIFO, one wave at a
  time**; inside a wave, ready jobs pack into free slots concurrently
  (biggest `gpu-require` first to reduce fragmentation; slots captured
  exclusively).
- CPU jobs (`gpu-require: 0`) are unlimited — they need no slot.
- **Modes**: with `llm_offload=true`, a queue containing any `gpu-require > 0`
  line runs **blocking** — the answer rides one SSE stream for the wave's
  life: status headers and a `: wave <id>` comment flush before the submit
  (a client or proxy never sits on a byteless wait), `notifications/progress`
  ride the stream when the client carries a `progressToken`, and the final
  JSON-RPC response closes it. A transport that cannot stream falls back to
  plain JSON. Everything
  else runs **async** — the answer returns immediately with the wave id.
  **Either mode, the wave belongs to the daemon**: a disconnect, a chat
  reload or a dropped request never kills it; cancellation is explicit only
  (shutdown or the red button below). A gone client loses nothing: the wave
  finishes and its result is stamped into the queue files (§6.1) — the
  consumer reads the file after reconnect instead of chasing the response.
- **Human progress feed**: the SSE `notifications/progress` payload carries
  `progressToken`, `message`, `markdown`, `done`/`total` and `wave` — the
  markdown line is a human-readable console for the GUI: running
  `▶ shot1 (cuda:0) 3/10`, done `[shot1](/nest/artifactory/proj/shot1.png) ✓ 2m27s`,
  failed `✖ shot1 — <error> <dur>`. A quiet stream emits an SSE comment
  keepalive (`: keepalive`) every 30 s — on every blocking stream, token or
  no token — so neither a proxy nor a client read timeout reaps a long
  single-job wave mid-render.
- **Red button**: `POST /wave/stop` (same port, outside MCP) cancels the
  newest unfinished wave — or `?wave=<id>` — killing its process groups;
  the wave ends not-ok, stamps its queue files as usual, and the VRAM drain
  (§8) runs on a fresh clock so the chat can reload into a quiet pool.
  Idempotent: a press on a finished or absent wave answers `stopped: false`.
  The GUI needs it because in XOR mode the LLM sleeps through the wave and
  cannot type "stop".
- Wave id: wall-clock stamp + daemon counter (`20261110-031222-01`) — unique
  across restarts without a ledger on disk.
- **Wave log (`--debug`)**: the jsonl event lines plus every job's streamed
  output (line-prefixed `<index> <tool> | `) go to a transient file in the
  system temp dir (`$TMPDIR/nerv-wave-<id>.log`); when the wave ends its
  tail (256 KB cap) is folded into the anchor queue file's `wave.debug`
  (a `debug_note` marks the truncation) and the temp file is deleted. The
  playground never holds log files; the ledger view carries the live `log`
  pointer only while the wave runs.
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
never touch the scheduler or the GPU:

### 7.1 `dice.roll`

`{notation: "4d6kh3+2", seed?: 42}`. Grammar: `NdM` terms, `kh`/`kl` keep
bounds, `+/-` constants; bounds 96 notation dice / 16 terms / 1000 count /
1e6 sides. Entropy: an explicit `seed` wins (deterministic, replayable);
otherwise a physical draw — `/dev/hwrng` when present, else the kernel CSPRNG
(rejection sampling, never modulo bias). The choice is never reported.

### 7.2 `calc.resolution`

Aspect string in, generator-friendly `WxH` out. Accepts `"16:9"`, `"16x9"`,
`"16×9"` or a decimal factor (`"1.5"`). Studio spec defaults: sides multiple
of 32, ~2 MP target, area floor 512², **no ceiling** (the consumer's canvas is
what the consumer asked for; a tool's own limits are the tool's business);
overridable per call (`multiple`, `targetMP`, `minSide`, `maxSide` — the last
one restores a ceiling when the caller wants it). Answer:
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
After a GPU wave the runner **drains the pool** before releasing it: reaped
children tear their driver contexts down asynchronously, so a reload fired at
once can cudaMalloc into still-held VRAM and OOM. The drain floors first,
polls the load of every queryable slot, and returns when the summed load goes
quiet or a ceiling passes — best-effort, bounded, invisible to the consumer
(the wave result is merely honest: the card is handed back truly idle).
