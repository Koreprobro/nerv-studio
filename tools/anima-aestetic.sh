#!/usr/bin/env sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Example nerv tool: anime/illustration generation via sd-cli
# (Anima aesthetic finetune). The pattern is general: any executable that
# answers --tool-info with a JSON interface, reads NERV_<FIELD> env vars on
# a real run and writes its declared output file is a valid nerv tool.
#
# Contract recap (SPECS.md §4–§5):
#   - sh <tool> --tool-info  prints the interface and exits (no generation);
#   - every declared param arrives as NERV_<FIELD> (hyphens folded to _);
#   - io-marked file params arrive FULL ABSOLUTE in both directions — the
#     core has created the output's parent dir and archived a same-name old
#     file before the run, so the script writes NERV_OUTPUT directly;
#   - this tool declares the calculator pair (aspect-ratio + target-mpx):
#     the core computes the generator-friendly canvas and injects
#     NERV_CALCULATED_WIDTH/HEIGHT — the consumer never does arithmetic;
#   - scheduler fields (gpu-require / gpu-backend / gpu-pin) are consumed by
#     the scheduler and never reach this env (device truth: CUDA_VISIBLE_DEVICES);
#   - exit 0 without the declared output file appearing == failed.

set -eu

DEFAULT_NEGATIVE="worst quality, low quality, blurry, jpeg artifacts, chromatic aberration"

case "${1:-}" in
  --tool-info)
    cat <<'JSON'
{
  "summary": "Anime/illustration, realism out of scope. Aestetic finetune, not for loras",
  "fields": [
    { "name": "output", "required": true, "io": "out", "description": "Output file relative to the playground root, WITH the .png extension (image/png)." },
    { "name": "prompt", "required": true, "description": "Image description (danbooru tags and/or natural English)." },
    { "name": "negative_prompt", "required": false, "description": "Your addition for default: worst quality, low quality, blurry, jpeg artifacts, chromatic aberration." },
    { "name": "aspect-ratio", "required": true, "description": "Canvas aspect ratio, \"W:H\" (e.g. \"16:9\", \"2x3\") or a decimal factor (\"1.5\"). Paired with target-mpx the core calculator computes the generator sides (multiple of 32) — read them through NERV_CALCULATED_WIDTH/HEIGHT." },
    { "name": "target-mpx", "required": false, "default": 1.0, "description": "Area target in megapixels for the calculator (anima is trained near 1 MP; raise it at your aesthetic risk)." },
    { "name": "seed", "required": false, "description": "Random seed. -1 (default) = random; pass the returned seed back to reproduce a result." },
    { "name": "cfg_scale", "required": false, "default": 5.0, "hidden": true, "description": "Guidance scale." },
    { "name": "sampler", "required": false, "default": "euler", "hidden": true, "description": "sd-cli sampling method." },
    { "name": "steps", "required": false, "default": 40, "hidden": true, "description": "Sampling steps." },
    { "name": "ckpt", "default": "/ai/Anima/split_files/diffusion_models/anima-aesthetic-v1.1.safetensors", "hidden": true, "description": "Diffusion checkpoint path (absolute). Debug override." },
    { "name": "text_encoder", "default": "/ai/Anima/split_files/text_encoders/qwen_3_06b_base.safetensors", "hidden": true, "description": "Text-encoder path (absolute). Debug override." },
    { "name": "vae", "default": "/ai/Anima/split_files/vae/qwen_image_vae.safetensors", "hidden": true, "description": "VAE path (absolute). Debug override." },
    { "name": "sd_backend", "default": "diffusion=cuda0,te=cuda0,vae=cuda0", "hidden": true, "description": "sd-cli --backend string (absolute device indices after scheduler pinning). Debug override." },
    { "name": "gpu-require", "default": 1, "description": "GPU slots to reserve (0 = CPU only)." }
  ]
}
JSON
    exit 0
    ;;
esac

# ===== generation logic =====
prompt="${NERV_PROMPT:?missing required param: NERV_PROMPT}"
negative="${NERV_NEGATIVE_PROMPT:+$NERV_NEGATIVE_PROMPT, }$DEFAULT_NEGATIVE"
seed="${NERV_SEED:--1}"
# hidden fields — invisible to the consumer-AI; the script is the authority
# and applies the same defaults its --tool-info declares.
cfg_scale="${NERV_CFG_SCALE:-5.0}"
sampler="${NERV_SAMPLER:-euler}"
steps="${NERV_STEPS:-40}"

# Canvas through the core calculator (SPECS §7.3): the tool declared the
# aspect-ratio + target-mpx pair, so the scheduler ran the studio resolution
# algorithm and injected the computed sides. The script never does math.
W="${NERV_CALCULATED_WIDTH:?missing NERV_CALCULATED_WIDTH: declare aspect-ratio + target-mpx and launch through the scheduler}"
H="${NERV_CALCULATED_HEIGHT:?missing NERV_CALCULATED_HEIGHT: declare aspect-ratio + target-mpx and launch through the scheduler}"

# NERV_OUTPUT is the absolute target — the core created its parent directory
# and archived an old same-name file before the run. The daemon is a black
# box to the tool: exit code + the declared output file existing afterwards.
out_path="$NERV_OUTPUT"

echo "anima: resolution=${W}x${H} seed=$seed prompt=\"$prompt\"" >&2

/usr/bin/sd-cli \
  --diffusion-model "${NERV_CKPT:-/ai/Anima/split_files/diffusion_models/anima-aesthetic-v1.1.safetensors}" \
  --llm "${NERV_TEXT_ENCODER:-/ai/Anima/split_files/text_encoders/qwen_3_06b_base.safetensors}" \
  --vae "${NERV_VAE:-/ai/Anima/split_files/vae/qwen_image_vae.safetensors}" \
  --lora-model-dir "${LORA_MODEL_DIR:-/ai/loras}" \
  --backend "${NERV_SD_BACKEND:-diffusion=cuda0,te=cuda0,vae=cuda0}" \
  --sage-attn \
  --offload-to-cpu \
  --prompt "$prompt" \
  --negative-prompt "$negative" \
  --width "$W" --height "$H" \
  --steps "$steps" \
  --cfg-scale "$cfg_scale" \
  --sampling-method "$sampler" \
  --seed "$seed" \
  --output "$out_path"

echo "anima: wrote $out_path (${W}x${H})" >&2
exit 0
