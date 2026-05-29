#!/bin/bash
# ─────────────────────────────────────────────────────────────────────────────
# HiSilver Desktop — Pre-required Tools Installer
#
# Installs system libraries needed for WebRTC camera/audio capture:
#   • pkg-config  — lets Go's CGo locate C libraries
#   • libvpx      — VP8 video encoder (camera → mobile)
#   • opus        — Opus audio codec  (mic → mobile)
#
# Supports: macOS (Homebrew) · Debian/Ubuntu (apt) · Fedora/RHEL (dnf)
# ─────────────────────────────────────────────────────────────────────────────

set -euo pipefail

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# ── Detect OS ─────────────────────────────────────────────────────────────────
detect_os() {
    if [[ "$(uname)" == "Darwin" ]]; then
        echo "macos"
    elif [[ -f /etc/debian_version ]]; then
        echo "debian"
    elif [[ -f /etc/fedora-release ]] || [[ -f /etc/redhat-release ]]; then
        echo "fedora"
    else
        echo "unknown"
    fi
}

OS=$(detect_os)
info "Detected OS: $OS"

# ── Already satisfied? ────────────────────────────────────────────────────────
check_already_installed() {
    if command -v pkg-config &>/dev/null \
        && pkg-config --exists vpx 2>/dev/null \
        && pkg-config --exists opus 2>/dev/null; then
        success "All required libraries are already installed."
        exit 0
    fi
}

check_already_installed

# ── Install ───────────────────────────────────────────────────────────────────
case "$OS" in

  macos)
    if ! command -v brew &>/dev/null; then
        error "Homebrew is not installed. Install it from https://brew.sh and re-run this script."
    fi
    info "Installing with Homebrew..."
    brew install pkg-config libvpx opus
    ;;

  debian)
    info "Installing with apt..."
    if [[ "$EUID" -ne 0 ]]; then
        SUDO="sudo"
    else
        SUDO=""
    fi
    $SUDO apt-get update -qq
    $SUDO apt-get install -y \
        pkg-config \
        libvpx-dev \
        libopus-dev \
        build-essential
    ;;

  fedora)
    info "Installing with dnf..."
    if [[ "$EUID" -ne 0 ]]; then
        SUDO="sudo"
    else
        SUDO=""
    fi
    $SUDO dnf install -y \
        pkgconf-pkg-config \
        libvpx-devel \
        opus-devel \
        gcc
    ;;

  *)
    error "Unsupported OS. Please install pkg-config, libvpx-dev, and libopus-dev manually."
    ;;
esac

# ── Verify ────────────────────────────────────────────────────────────────────
echo ""
info "Verifying installation..."

FAILED=0

if command -v pkg-config &>/dev/null; then
    success "pkg-config  $(pkg-config --version)"
else
    warn "pkg-config not found"
    FAILED=1
fi

if pkg-config --exists vpx 2>/dev/null; then
    success "libvpx      $(pkg-config --modversion vpx)"
else
    warn "libvpx not found via pkg-config"
    FAILED=1
fi

if pkg-config --exists opus 2>/dev/null; then
    success "opus        $(pkg-config --modversion opus)"
else
    warn "libopus not found via pkg-config"
    FAILED=1
fi

echo ""
if [[ $FAILED -eq 0 ]]; then
    success "All prerequisites are ready. You can now run: ./start.sh"
else
    error "Some libraries could not be verified. Check the output above."
fi
