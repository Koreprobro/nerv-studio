#!/usr/bin/env sh
# tools/example/scene.sh — minimal self-documenting tool template for nerv-core.
#
# nerv-core's MCP server calls this script with `--tool-info` on demand
# (tools.list / tools.detail), NOT at startup. The `--tool-info` block MUST NOT run
# any generation logic — it only describes the tool so the MCP server can build a schema.
#
# Convention (see AGENTS.md §2.13):
#   - public params arrive as env vars: NERV_<FIELD> (uppercased)
#   - output: NERV_OUTPUT — the file to write, artifactory-relative WITH
#   extension; nerv_out joins it with $NERV_ARTIFACTORY_DIR and archives
#   an existing same-name file first (helpers: tools/_nerv.inc)
#   - exit code 0 = ok, non-zero = failed

case "${1:-}" in
  --tool-info)
    # Machine-readable (JSON) self-description.
    #   - "fields":    params; a field may be marked "private": true — a
    #     human-side knob (cfg / sampler / steps & friends, GUI workbench).
    #     Private fields MUST carry "default"; the MCP view shown to the
    #     consumer-AI drops private entries SILENTLY (SPECS.md §3.1), so the
    #     script must apply the same default when its NERV_<FIELD> is unset.
    #     Params are never validated against this list — the GUI may pass
    #     private values through untouched.
    #   - a field may be marked "file": true — its value (a path or an array
    #     of paths) is an artifactory-relative FILE the job needs; the
    #     scheduler waits for the file to exist before running the job.
    cat <<'JSON'
{
  "summary": "Synthesize a scene image (SDXL backend).",
  "fields": [
    { "name": "output", "required": true, "description": "Output file relative to the artifactory root, WITH the .png extension (image/png)." },
    { "name": "prompt",        "required": true,  "description": "Text description of the scene." },
    { "name": "negative_prompt", "required": false, "description": "What to avoid in the image." },
    { "name": "aspect_ratio",  "required": false, "description": "One of 16:9, 2:3, 1:1, 3:2, 21:9." },
    { "name": "cfg",           "required": false, "private": true, "default": 7.0,  "description": "Guidance scale." },
    { "name": "sampler",       "required": false, "private": true, "default": "euler_a", "description": "Sampler name." },
    { "name": "steps",         "required": false, "private": true, "default": 30,   "description": "Sampling steps." }
  ],
  "gpu-count": 1
}
JSON
    exit 0
    ;;
esac

# shared helpers: nerv_out (output path + same-name archive), nerv_in (inputs)
_d=$(dirname "$0"); while [ ! -f "$_d/_nerv.inc" ] && [ "$_d" != / ]; do _d=$(dirname "$_d"); done
. "$_d/_nerv.inc"

# ===== Real generation logic (author-defined) =====
# Params arrive as env vars: $NERV_PROMPT, $NERV_NEGATIVE_PROMPT, $NERV_ASPECT_RATIO
# and the private ones: $NERV_CFG, $NERV_SAMPLER, $NERV_STEPS — the consumer-AI
# never sees the private names, so the script MUST default them itself
# (same values as declared in --tool-info).
# Output: nerv_out resolves NERV_OUTPUT (params.output, WITH extension) under
# $NERV_ARTIFACTORY_DIR and archives any old same-name file first.
# Exit 0 = success, non-zero = failure. nerv-core checks that the declared
# output file exists after the run (it never inspects content).
#
#   set -eu
#   prompt="${NERV_PROMPT:?missing required param: prompt}"
#   ratio="${NERV_ASPECT_RATIO:-16:9}"
#   cfg="${NERV_CFG:-7.0}"; sampler="${NERV_SAMPLER:-euler_a}"; steps="${NERV_STEPS:-30}"
#   ... call the consumer's backend, produce an image ...
#   out="$(nerv_out)"; mv result.png "$out"
#
echo "scene.sh: real logic not implemented in this template." >&2
exit 1
