#!/usr/bin/env bash
# drover installer — prebuilt binary from GitHub Releases.
#   curl -fsSL https://raw.githubusercontent.com/notshekhar/drover/main/install.sh | bash
#
# drover is a single static Go binary with no cgo, so there is no runtime to
# install, no glibc/musl split, and nothing to build on your machine.
#
# Layout after install:
#   $DROVER_BIN_HOME/            (default: ~/.drover-bin)
#     └── drover                 (the executable)
#   $BIN_DIR/drover  → symlink
#
# Note the install dir is ~/.drover-bin, NOT ~/.drover. The latter is drover's
# data directory — its objects and checkouts — and the installer never touches
# it, so reinstalling cannot lose what you have applied.
#
# Flags (curl | bash -s -- <flags>) — each maps to the env knob next to it:
#   -v, --version <vX.Y.Z>   pin a specific tag        (DROVER_VERSION)
#       --force              skip up-to-date gate      (DROVER_FORCE=1)
#       --uninstall          remove install + links    (DROVER_UNINSTALL=1)
#       --no-modify-path     don't touch shell rc      (DROVER_NO_MODIFY_PATH=1)
#   -h, --help
#
# Extra env knobs:
#   DROVER_REPO_SLUG   notshekhar/drover   override repo
#   DROVER_BIN_HOME    $HOME/.drover-bin   install dir
#   DROVER_BIN_DIR                         symlink dir (auto: /usr/local/bin
#                                          or $HOME/.local/bin)

set -euo pipefail

REPO_SLUG="${DROVER_REPO_SLUG:-notshekhar/drover}"
BIN_HOME="${DROVER_BIN_HOME:-$HOME/.drover-bin}"
FORCE="${DROVER_FORCE:-0}"
UNINSTALL="${DROVER_UNINSTALL:-0}"
PIN_VERSION="${DROVER_VERSION:-}"
NO_MODIFY_PATH="${DROVER_NO_MODIFY_PATH:-0}"

usage() {
  cat <<EOF
drover installer

Usage: install.sh [options]

Options:
  -v, --version <vX.Y.Z>  Install a specific release
      --force             Reinstall even when up to date
      --uninstall         Remove the install and its symlink
      --no-modify-path    Don't write the PATH line to your shell rc
  -h, --help              Show this help

Examples:
  curl -fsSL https://raw.githubusercontent.com/${REPO_SLUG}/main/install.sh | bash
  curl -fsSL https://raw.githubusercontent.com/${REPO_SLUG}/main/install.sh | bash -s -- --version v0.1.0
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    -v|--version)
      if [ -n "${2:-}" ]; then PIN_VERSION="$2"; shift 2; else
        printf "\033[31m--version requires an argument\033[0m\n" >&2; exit 1; fi ;;
    --force) FORCE=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    --no-modify-path) NO_MODIFY_PATH=1; shift ;;
    *) printf "\033[2mignoring unknown option: %s\033[0m\n" "$1" >&2; shift ;;
  esac
done

bold() { printf "\033[1m%s\033[0m\n" "$*"; }
dim()  { printf "\033[2m%s\033[0m\n" "$*"; }
err()  { printf "\033[31m%s\033[0m\n" "$*" >&2; }

need_tool() {
  local cmd="$1" hint="$2"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    err "Missing required tool: $cmd"
    err "  → $hint"
    exit 1
  fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    err "missing sha256sum/shasum"; return 1
  fi
}

ver_gt() {
  local a="${1#v}" b="${2#v}"
  [ "$a" = "$b" ] && return 1
  local top
  top="$(printf '%s\n%s\n' "$a" "$b" | sort -V | head -n1)"
  [ "$top" = "$b" ] && return 0
  return 1
}

# ── Download progress bar ──────────────────────────────────────────────────
# curl writes a --trace-ascii stream into a FIFO; we parse content-length and
# `<= recv data` records live and draw a ■■■･･･ 42% bar on stderr. Only used
# when stderr is a TTY; anything else (or any failure) falls back to plain
# curl in the caller.

# sed with unbuffered output — GNU (-u), BSD/macOS (-l), else pad each line
# past the libc buffer so records flush through the pipe as they happen.
unbuffered_sed() {
  if echo | sed -u -e "" >/dev/null 2>&1; then
    sed -nu "$@"
  elif echo | sed -l -e "" >/dev/null 2>&1; then
    sed -nl "$@"
  else
    local pad="$(printf "\n%512s" "")"
    sed -ne "s/$/\\${pad}/" "$@"
  fi
}

PROGRESS_COLOR='\033[38;5;215m'
PROGRESS_NC='\033[0m'

print_progress() {
  local bytes="$1" length="$2"
  [ "$length" -gt 0 ] || return 0

  local width=50
  local percent=$(( bytes * 100 / length ))
  [ "$percent" -gt 100 ] && percent=100
  local on=$(( percent * width / 100 ))
  local off=$(( width - on ))

  local filled=$(printf "%*s" "$on" "")
  filled=${filled// /■}
  local empty=$(printf "%*s" "$off" "")
  empty=${empty// /･}

  printf "\r${PROGRESS_COLOR}%s%s %3d%%${PROGRESS_NC}" "$filled" "$empty" "$percent" >&4
}

download_with_progress() {
  local url="$1" output="$2"

  if [ -t 2 ]; then
    exec 4>&2
  else
    exec 4>/dev/null
  fi

  local tmp_dir="${TMPDIR:-/tmp}"
  local tracefile="${tmp_dir}/drover_install_$$.trace"

  rm -f "$tracefile"
  mkfifo "$tracefile" 2>/dev/null || return 1

  # Hide the cursor while the bar redraws; always restore it on the way out.
  printf "\033[?25l" >&4
  trap "trap - RETURN; rm -f \"$tracefile\"; printf '\033[?25h' >&4; exec 4>&-" RETURN

  # -f so an HTTP error fails the download (and the caller's fallback runs)
  # instead of tracing a 404 page into the output file.
  (
    curl -f --trace-ascii "$tracefile" -s -L -o "$output" "$url"
  ) &
  local curl_pid=$!

  unbuffered_sed \
    -e 'y/ACDEGHLNORTV/acdeghlnortv/' \
    -e '/^0000: content-length:/p' \
    -e '/^<= recv data/p' \
    "$tracefile" | \
  {
    local length=0 bytes=0

    while IFS=" " read -r -a line; do
      [ "${#line[@]}" -lt 2 ] && continue
      local tag="${line[0]} ${line[1]}"

      if [ "$tag" = "0000: content-length:" ]; then
        # Each response in a redirect chain restarts the count; the final
        # (asset) response's length is the one the bar ends up tracking.
        length="${line[2]}"
        length=$(echo "$length" | tr -d '\r')
        bytes=0
      elif [ "$tag" = "<= recv" ]; then
        local size="${line[3]}"
        bytes=$(( bytes + size ))
        if [ "$length" -gt 0 ]; then
          print_progress "$bytes" "$length"
        fi
      fi
    done
  }

  wait $curl_pid
  local ret=$?
  echo "" >&4
  return $ret
}

# ── Detect target ─────────────────────────────────────────────────────────
detect_target() {
  local uname_s uname_m os arch
  uname_s="$(uname -s)"
  uname_m="$(uname -m)"
  case "$uname_s" in
    Darwin) os="darwin" ;;
    Linux)  os="linux" ;;
    FreeBSD) os="freebsd" ;;
    MINGW*|MSYS*|CYGWIN*)
      err "Detected Git Bash / MSYS on Windows. Use the PowerShell installer instead:"
      err "  irm https://raw.githubusercontent.com/${REPO_SLUG}/main/install.ps1 | iex"
      exit 1
      ;;
    *)      err "unsupported OS: $uname_s"; exit 1 ;;
  esac
  case "$uname_m" in
    x86_64|amd64)   arch="x64" ;;
    arm64|aarch64)  arch="arm64" ;;
    *)              err "unsupported arch: $uname_m"; exit 1 ;;
  esac
  # A shell under Rosetta reports x86_64 on Apple Silicon — install the
  # native arm64 build instead of the emulated one.
  if [ "$os" = "darwin" ] && [ "$arch" = "x64" ]; then
    if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = "1" ]; then
      arch="arm64"
    fi
  fi
  # FreeBSD ships one arch here; arm64 has no release asset.
  if [ "$os" = "freebsd" ] && [ "$arch" != "x64" ]; then
    err "unsupported target: freebsd-$arch"; exit 1
  fi
  printf "%s-%s" "$os" "$arch"
}

# No libc check: the binaries are CGO_ENABLED=0 static builds, so Alpine and
# every other musl distro runs the same asset as glibc ones.

# ── Resolve latest release tag ────────────────────────────────────────────
# Prefer the releases/latest redirect — it isn't subject to the anonymous
# GitHub API rate limit (60 req/h/IP) that bites CI and shared networks.
# Fall back to the API if redirect parsing fails.
resolve_latest_tag() {
  local final tag
  final="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/${REPO_SLUG}/releases/latest" 2>/dev/null || true)"
  tag="${final##*/}"
  case "$tag" in
    v[0-9]*) printf "%s" "$tag"; return 0 ;;
  esac
  curl -fsSL "https://api.github.com/repos/${REPO_SLUG}/releases/latest" 2>/dev/null \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1
}

installed_version() {
  local exe="$BIN_HOME/drover"
  [ -x "$exe" ] || return 1
  "$exe" version 2>/dev/null | awk '{print $2}' | head -n1
}

# ── Where to symlink ──────────────────────────────────────────────────────
pick_bin_dir() {
  if [ -n "${DROVER_BIN_DIR:-}" ]; then
    printf "%s" "$DROVER_BIN_DIR"; return
  fi
  # /usr/local/bin when it is writable without sudo; otherwise a user dir,
  # because prompting for a password inside a piped installer is hostile.
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    printf "/usr/local/bin"; return
  fi
  printf "%s" "$HOME/.local/bin"
}

shell_rc() {
  case "$(basename "${SHELL:-/bin/bash}")" in
    zsh)  printf "%s" "$HOME/.zshrc" ;;
    bash)
      # macOS bash reads .bash_profile for login shells; Linux reads .bashrc.
      if [ "$(uname -s)" = "Darwin" ] && [ -f "$HOME/.bash_profile" ]; then
        printf "%s" "$HOME/.bash_profile"
      else
        printf "%s" "$HOME/.bashrc"
      fi ;;
    fish) printf "%s" "$HOME/.config/fish/config.fish" ;;
    *)    printf "%s" "$HOME/.profile" ;;
  esac
}

ensure_on_path() {
  local dir="$1"
  case ":$PATH:" in *":$dir:"*) return 0 ;; esac
  [ "$NO_MODIFY_PATH" = "1" ] && {
    dim "  $dir is not on PATH (--no-modify-path, so leaving your rc alone)"
    return 0
  }

  local rc; rc="$(shell_rc)"
  local line
  if [ "$(basename "$rc")" = "config.fish" ]; then
    line="fish_add_path $dir"
  else
    line="export PATH=\"$dir:\$PATH\""
  fi

  mkdir -p "$(dirname "$rc")"
  if [ -f "$rc" ] && grep -Fq "$line" "$rc" 2>/dev/null; then
    return 0
  fi
  printf '\n# drover\n%s\n' "$line" >> "$rc"
  dim "  added $dir to PATH in $rc"
  NEEDS_RELOAD="$rc"
}

# ── Uninstall ─────────────────────────────────────────────────────────────
do_uninstall() {
  bold "▶ Removing drover"
  local bin_dir; bin_dir="$(pick_bin_dir)"
  for p in "$bin_dir/drover" "/usr/local/bin/drover" "$HOME/.local/bin/drover"; do
    if [ -L "$p" ] || [ -e "$p" ]; then
      rm -f "$p" 2>/dev/null && dim "  removed $p" || true
    fi
  done
  if [ -d "$BIN_HOME" ]; then
    rm -rf "$BIN_HOME" && dim "  removed $BIN_HOME"
  fi
  bold "✓ drover removed"
  # ~/.drover holds applied objects and checkouts. Deleting someone's data on
  # an uninstall would be unforgivable, so it is only ever mentioned.
  if [ -d "$HOME/.drover" ]; then
    dim "  your data is still at ~/.drover (objects and checkouts); delete it yourself if you want it gone"
  fi
}

# ── Install ───────────────────────────────────────────────────────────────
do_install() {
  need_tool curl "install curl"
  need_tool tar  "install tar"

  local target latest installed
  target="$(detect_target)"
  bold "▶ drover installer"
  dim "  target: $target"

  if [ -n "$PIN_VERSION" ]; then
    latest="$PIN_VERSION"
    case "$latest" in v*) ;; *) latest="v$latest" ;; esac
  else
    latest="$(resolve_latest_tag || true)"
  fi
  if [ -z "$latest" ]; then
    err "could not resolve a release tag for ${REPO_SLUG}"
    err "  is there a published release yet?"
    exit 1
  fi

  if installed="$(installed_version)" && [ -n "$installed" ]; then
    if [ "$FORCE" != "1" ] && ! ver_gt "${latest#v}" "$installed"; then
      bold "✓ Up to date (installed $installed, latest $latest)"
      dim "  DROVER_FORCE=1 to reinstall"
      exit 0
    fi
    dim "  update: $installed → ${latest#v}"
  else
    dim "  installing ${latest#v}"
  fi

  local scratch tarball sum url base
  rm -rf "${BIN_HOME}".old.* "${BIN_HOME}".new.* 2>/dev/null || true
  scratch="${BIN_HOME}.new.$$"
  trap 'rm -rf "$scratch" 2>/dev/null || true' EXIT
  mkdir -p "$scratch"

  base="https://github.com/${REPO_SLUG}/releases/download/${latest}"
  url="${base}/drover-${target}.tar.gz"
  tarball="$scratch/drover.tar.gz"
  sum="$scratch/drover.tar.gz.sha256"

  bold "▶ Downloading ${url##*/}"
  # Fancy ■■■･･･ 42% bar on a TTY; plain curl everywhere else (non-TTY, or
  # if the traced download fails for any reason — including HTTP errors,
  # where the retry surfaces curl's own message).
  if ! { [ -t 2 ] && download_with_progress "$url" "$tarball"; }; then
    if ! curl -fL --progress-bar "$url" -o "$tarball"; then
      err "download failed: $url"
      err "  the release may not have a $target asset"
      exit 1
    fi
  fi

  if curl -fsSL "${url}.sha256" -o "$sum" 2>/dev/null && [ -s "$sum" ]; then
    local expected got
    expected="$(awk '{print $1}' "$sum")"
    got="$(sha256_of "$tarball")"
    if [ "$expected" != "$got" ]; then
      err "sha256 mismatch (expected $expected, got $got)"
      exit 1
    fi
    dim "  sha256 ok"
  else
    dim "  sha256 file missing — skipping verify"
  fi

  bold "▶ Extracting"
  tar -xzf "$tarball" -C "$scratch"
  if [ ! -x "$scratch/$target/drover" ]; then
    err "tarball missing $target/drover"
    exit 1
  fi

  # Defensive: clear quarantine if anything in the chain set it (Gatekeeper
  # blocks unsigned quarantined binaries with a scary dialog).
  if [ "$(uname -s)" = "Darwin" ] && command -v xattr >/dev/null 2>&1; then
    xattr -dr com.apple.quarantine "$scratch/$target" 2>/dev/null || true
  fi

  swap_into_place "$scratch/$target"
  trap - EXIT
  rm -rf "$scratch" 2>/dev/null || true

  # `drover upgrade` looks for this before replacing the binary: without it,
  # drover arrived some other way (go install, a package manager) and
  # upgrading in place would leave two copies with no clear owner.
  printf "binary\n" > "$BIN_HOME/.install-method" 2>/dev/null || true

  link_globally
  smoke_test
  finish_message "$latest"
}

# ── Atomic swap install dir ───────────────────────────────────────────────
swap_into_place() {
  local src="$1"
  bold "▶ Installing to $BIN_HOME"
  mkdir -p "$(dirname "$BIN_HOME")"
  local backup=""
  if [ -e "$BIN_HOME" ]; then
    backup="${BIN_HOME}.old.$$"
    mv "$BIN_HOME" "$backup"
  fi
  mv "$src" "$BIN_HOME"
  [ -n "$backup" ] && rm -rf "$backup" 2>/dev/null || true
}

link_globally() {
  local bin_dir; bin_dir="$(pick_bin_dir)"
  bold "▶ Linking drover into $bin_dir"
  mkdir -p "$bin_dir"
  ln -sf "$BIN_HOME/drover" "$bin_dir/drover"
  ensure_on_path "$bin_dir"
}

smoke_test() {
  if ! "$BIN_HOME/drover" version >/dev/null 2>&1; then
    err "the installed binary did not run"
    err "  try: $BIN_HOME/drover version"
    exit 1
  fi
}

NEEDS_RELOAD=""

finish_message() {
  local tag="$1"
  printf "\n"
  bold "✓ drover ${tag#v} installed"
  printf "\n"
  dim "  start the engine:"
  printf "    drover serve\n"
  dim "  then, in another terminal, give it a repository:"
  printf "    drover apply -f repo.yaml\n"
  dim "  and point an agent at it:"
  printf "    claude mcp add --transport http drover http://127.0.0.1:7432/mcp\n"
  if [ -n "$NEEDS_RELOAD" ]; then
    printf "\n"
    dim "  open a new shell, or: source $NEEDS_RELOAD"
  fi
  printf "\n"
}

if [ "$UNINSTALL" = "1" ]; then
  do_uninstall
else
  do_install
fi
