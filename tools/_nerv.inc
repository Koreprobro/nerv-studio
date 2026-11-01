# _nerv.inc — shared sh-side helpers for nerv tool scripts (contract M10,
# SPECS.md §3.1). Source it AFTER the `--tool-info` case block (the probe runs
# with a bare environment).
#
# Environment the scheduler guarantees at generation time:
#   NERV_ARTIFACTORY_DIR  artifactory root (absolute)
#   NERV_OUTPUT           declared output file, artifactory-relative, WITH
#                         extension — the consumer-AI picked the name
#   NERV_<FIELD>          every job param; fields declared "file": true carry
#                         artifactory-relative input paths (comma-separated
#                         when the field is an array)
#
# Usage:
#   out="$(nerv_out)"     # absolute target path; an existing same-name file is
#                         # archived to <name>.1, <name>.2, ... first, so the
#                         # consumer always gets the exact name it asked for
#   in="$(nerv_in "$NERV_INPUT")"   # absolute path of a declared input file

# nerv_archive <existing-path> — move a file aside under an indexed name.
nerv_archive() {
	_nai=1
	while [ -e "$1.${_nai}" ]; do _nai=$((_nai + 1)); done
	mv "$1" "$1.${_nai}"
}

# nerv_out — resolve the declared output under the artifactory root, create its
# parent directory, archive any file already living there, print the path.
nerv_out() {
	if [ -z "${NERV_ARTIFACTORY_DIR:-}" ] || [ -z "${NERV_OUTPUT:-}" ]; then
		echo "nerv: NERV_ARTIFACTORY_DIR and NERV_OUTPUT are required" >&2
		return 1
	fi
	case "$NERV_OUTPUT" in
	/*)
		echo "nerv: NERV_OUTPUT must be artifactory-relative (got $NERV_OUTPUT)" >&2
		return 1
		;;
	esac
	case "/$NERV_OUTPUT/" in
	*/../*)
		echo "nerv: NERV_OUTPUT must stay inside the artifactory (got $NERV_OUTPUT)" >&2
		return 1
		;;
	esac
	_no="$NERV_ARTIFACTORY_DIR/$NERV_OUTPUT"
	mkdir -p "$(dirname "$_no")" || return 1
	[ -e "$_no" ] && nerv_archive "$_no"
	printf '%s\n' "$_no"
}

# nerv_in <artifactory-relative-path> — absolute path under the root.
nerv_in() {
	case "$1" in
	/*)
		echo "nerv: input path must be artifactory-relative (got $1)" >&2
		return 1
		;;
	esac
	case "/$1/" in
	*/../*)
		echo "nerv: input path must stay inside the artifactory (got $1)" >&2
		return 1
		;;
	esac
	printf '%s\n' "$NERV_ARTIFACTORY_DIR/$1"
}

# nerv_batch_fix <out-path> <count> — repair the sd-cli batch naming. With
# count > 1 and no %d in --output, sd-cli saves <stem>_0.<ext>, <stem>_1.<ext>,
# ... instead of the exact declared name (examples/cli/main.cpp save_results):
# the core would see exit 0 with the declared output missing and fail the job.
# The primary take (_0) is moved onto the declared name; the extra takes stay
# next to it as <stem>_<i>.<ext> siblings for the consumer to browse.
nerv_batch_fix() {
	[ "${2:-1}" -gt 1 ] 2>/dev/null || return 0
	_bf_stem="${1%.*}"
	_bf_ext="${1##*.}"
	[ "$_bf_stem" != "$1" ] || return 0
	[ -f "${_bf_stem}_0.${_bf_ext}" ] && mv "${_bf_stem}_0.${_bf_ext}" "$1"
	return 0
}