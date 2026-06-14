#!/bin/bash

# Build script for MihomoMate Android AAR
# Usage: ./build_android.sh [arm64|arm|x86_64|all]

set -e

# Configuration
PACKAGE="github.com/metacubex/mihomo/mate"
LIBRARY_NAME="mate"
OUTPUT_DIR="./build/android"
TEMP_DIR="./build/android/.temp"
WORK_TMP_DIR=""
GOMOBILE_CACHE_DIR="./build/gomobile"
BUILD_TAGS="with_gvisor,cmfa"
BUILD_TYPE="${1:-all}"

# Android SDK path (auto-detect or set manually)
ANDROID_SDK_ROOT="${ANDROID_SDK_ROOT:-$HOME/Android/Sdk}"
ANDROID_NDK_VERSION="25.2.9519653"
ANDROID_NDK_ROOT="$ANDROID_SDK_ROOT/ndk/$ANDROID_NDK_VERSION"
ANDROID_MIN_SDK=21

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

echo_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
echo_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
echo_error() { echo -e "${RED}[ERROR]${NC} $1"; }

cleanup_work_tmp() {
    if [ -n "$WORK_TMP_DIR" ] && [ -d "$WORK_TMP_DIR" ]; then
        rm -rf "$WORK_TMP_DIR"
    fi
}
trap cleanup_work_tmp EXIT

# Check Android SDK
check_android_sdk() {
    if [ ! -d "$ANDROID_SDK_ROOT" ]; then
        echo_error "Android SDK not found at: $ANDROID_SDK_ROOT"
        echo_error "Please set ANDROID_SDK_ROOT environment variable"
        exit 1
    fi
    
    if [ ! -d "$ANDROID_NDK_ROOT" ]; then
        echo_error "Android NDK not found at: $ANDROID_NDK_ROOT"
        echo_error "Please install NDK version $ANDROID_NDK_VERSION via Android Studio SDK Manager"
        exit 1
    fi
    
    echo_info "Android SDK: $ANDROID_SDK_ROOT"
    echo_info "Android NDK: $ANDROID_NDK_ROOT"
}

# Clean output directory
echo_info "Cleaning build directory..."
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"
mkdir -p "$TEMP_DIR"

# Setup temp directories
echo_info "Preparing local build temp/cache directories..."
WORK_TMP_DIR="$(mktemp -d "/tmp/mihomo-gomobile-android.XXXXXX")"
mkdir -p "$GOMOBILE_CACHE_DIR"
export TMPDIR="$(cd "$WORK_TMP_DIR" && pwd)/"
export GOMOBILE="$(cd "$GOMOBILE_CACHE_DIR" && pwd)"

# Ensure gomobile is installed
ensure_gomobile() {
    if ! command -v gomobile &> /dev/null; then
        echo_info "Installing gomobile..."
        go install golang.org/x/mobile/cmd/gomobile@latest
        go install golang.org/x/mobile/cmd/gobind@latest
        gomobile init
    fi
    
    MOBILE_DIR="$(go env GOPATH)/src/golang.org/x/mobile"
    if [ ! -d "$MOBILE_DIR/bind" ]; then
        echo_info "golang.org/x/mobile/bind not found. Installing..."
        mkdir -p "$MOBILE_DIR"
        git clone --depth 1 https://go.googlesource.com/mobile "$MOBILE_DIR"
    fi
    
    # Check for go.work
    if [ ! -f "go.work" ]; then
        echo_warn "go.work not found. Creating one..."
        cat > go.work << EOF
go 1.25.0

use (
    .
    $MOBILE_DIR
)
EOF
    fi
}

# Build for Android
build_android() {
    local target_arch="$1"
    local target_flag=""
    local output_suffix=""
    
    case "$target_arch" in
        arm64)
            target_flag="android/arm64"
            output_suffix="arm64-v8a"
            ;;
        arm)
            target_flag="android/arm"
            output_suffix="armeabi-v7a"
            ;;
        x86_64)
            target_flag="android/amd64"
            output_suffix="x86_64"
            ;;
        x86)
            target_flag="android/386"
            output_suffix="x86"
            ;;
        *)
            echo_error "Unknown architecture: $target_arch"
            return 1
            ;;
    esac
    
    echo_info "Building Android AAR for $target_arch..."
    
    local build_output="$TEMP_DIR/${output_suffix}"
    mkdir -p "$build_output"
    
    CGO_ENABLED=1 \
    gomobile bind \
        -androidapi=$ANDROID_MIN_SDK \
        -tags="$BUILD_TAGS" \
        -target="$target_flag" \
        -o "$build_output/${LIBRARY_NAME}.aar" \
        -ldflags='-s -w' \
        "$PACKAGE"
    
    echo_info "✅ Built AAR for $target_arch at $build_output/${LIBRARY_NAME}.aar"
}

# Build for all architectures and combine into single AAR
build_all() {
    echo_info "Building Android AAR for all architectures..."
    
    local all_aars=()
    
    for arch in arm64 arm x86_64; do
        build_android "$arch"
        local suffix=""
        case "$arch" in
            arm64) suffix="arm64-v8a" ;;
            arm) suffix="armeabi-v7a" ;;
            x86_64) suffix="x86_64" ;;
        esac
        all_aars+=("$TEMP_DIR/$suffix/${LIBRARY_NAME}.aar")
    done
    
    echo_info "Merging AAR files into universal AAR..."
    
    local universal_dir="$TEMP_DIR/universal"
    mkdir -p "$universal_dir"
    
    # Extract all AARs and merge
    local first_aar="${all_aars[0]}"
    unzip -q "$first_aar" -d "$universal_dir"
    
    # Merge native libraries from other AARs
    for aar in "${all_aars[@]:1}"; do
        local arch_name=$(basename $(dirname "$aar"))
        local extract_dir="$TEMP_DIR/extract_$arch_name"
        mkdir -p "$extract_dir"
        unzip -q "$aar" -d "$extract_dir"
        
        # Copy .so files
        if [ -d "$extract_dir/jni" ]; then
            mkdir -p "$universal_dir/jni"
            cp -R "$extract_dir/jni/"* "$universal_dir/jni/"
        fi
    done
    
    # Repackage as AAR
    cd "$universal_dir"
    zip -q -r "$OUTPUT_DIR/${LIBRARY_NAME}.aar" .
    cd - > /dev/null
    
    echo_info "✅ Universal Android AAR created: $OUTPUT_DIR/${LIBRARY_NAME}.aar"
}

# Main build logic
check_android_sdk
ensure_gomobile

case "$BUILD_TYPE" in
    arm64|arm|x86_64|x86)
        build_android "$BUILD_TYPE"
        mkdir -p "$OUTPUT_DIR"
        cp "$TEMP_DIR"/*/"${LIBRARY_NAME}.aar" "$OUTPUT_DIR/" 2>/dev/null || true
        ;;
    all)
        build_all
        ;;
    *)
        echo_error "Unknown build type: $BUILD_TYPE"
        echo "Usage: $0 [arm64|arm|x86_64|x86|all]"
        echo ""
        echo "Options:"
        echo "  arm64   - Build for ARM64 (arm64-v8a)"
        echo "  arm     - Build for ARM (armeabi-v7a)"
        echo "  x86_64  - Build for x86_64"
        echo "  x86     - Build for x86"
        echo "  all     - Build universal AAR with all architectures (recommended)"
        exit 1
        ;;
esac

echo ""
echo_info "🎉 Build completed!"
echo ""
echo_info "Generated AAR:"
ls -lh "$OUTPUT_DIR"/*.aar 2>/dev/null || echo_warn "  No AAR found"
echo ""
echo_info "To use in Android Studio:"
echo_info "  1. Copy the .aar file to your project's libs directory"
echo_info "  2. Add to build.gradle: implementation files('libs/mate.aar')"
echo_info "  3. Import in Kotlin/Java: import mate.*"
