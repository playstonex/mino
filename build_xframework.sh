#!/bin/bash

# Build script for MihomoMate XCFramework
# Usage: ./build_xframework.sh [ios|macos|all|unified]

set -e

# Configuration
PACKAGE="github.com/metacubex/mihomo/mate"
FRAMEWORK_NAME="Mate"
OUTPUT_DIR="./build/xframework"
TEMP_DIR="./build/xframework/.temp"
WORK_TMP_DIR=""
GOMOBILE_CACHE_DIR="./build/gomobile"
BUILD_TAGS="with_gvisor"
BUILD_TYPE="${1:-unified}"

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

# Normalize gomobile iOS framework naming so all slices expose a single Swift module: Mate.
normalize_ios_framework_slice() {
    local slice_dir="$1"
    local framework_dir
    framework_dir="$(find "$slice_dir" -maxdepth 1 -type d -name "*.framework" | head -n 1)"

    if [ -z "$framework_dir" ]; then
        echo_error "No framework found in iOS slice: $slice_dir"
        exit 1
    fi

    local original_name
    original_name="$(basename "$framework_dir" .framework)"

    if [ "$original_name" != "$FRAMEWORK_NAME" ]; then
        mv "$framework_dir" "$slice_dir/${FRAMEWORK_NAME}.framework"
        framework_dir="$slice_dir/${FRAMEWORK_NAME}.framework"
    fi

    if [ -f "$framework_dir/$original_name" ]; then
        mv "$framework_dir/$original_name" "$framework_dir/$FRAMEWORK_NAME"
    fi

    if [ -f "$framework_dir/Headers/${original_name}.h" ]; then
        mv "$framework_dir/Headers/${original_name}.h" "$framework_dir/Headers/${FRAMEWORK_NAME}.h"
        sed -i '' "s/${original_name}/${FRAMEWORK_NAME}/g" "$framework_dir/Headers/${FRAMEWORK_NAME}.h"
    fi

    if [ -f "$framework_dir/Modules/module.modulemap" ]; then
        sed -i '' -E \
            "s/^framework module \"[^\"]+\"/framework module \"${FRAMEWORK_NAME}\"/" \
            "$framework_dir/Modules/module.modulemap"
        sed -i '' \
            "s/header \"${original_name}.h\"/header \"${FRAMEWORK_NAME}.h\"/" \
            "$framework_dir/Modules/module.modulemap"
    fi

    if [ -f "$framework_dir/Info.plist" ]; then
        /usr/libexec/PlistBuddy -c "Set :CFBundleExecutable $FRAMEWORK_NAME" "$framework_dir/Info.plist" >/dev/null
        /usr/libexec/PlistBuddy -c "Set :CFBundleIdentifier ${FRAMEWORK_NAME}-ios" "$framework_dir/Info.plist" >/dev/null
    fi
}

# Clean output directory
echo_info "Cleaning build directory..."
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"
mkdir -p "$TEMP_DIR"

# Keep gomobile cache in workspace but place temp work outside the Go module tree.
echo_info "Preparing local build temp/cache directories..."
rm -rf "./build/xframework/tmp"
rm -rf "./build/xframework/.tmp"
WORK_TMP_DIR="$(mktemp -d "/tmp/mihomo-gomobile-work.XXXXXX")"
mkdir -p "$GOMOBILE_CACHE_DIR"
export TMPDIR="$(cd "$WORK_TMP_DIR" && pwd)/"
export GOMOBILE="$(cd "$GOMOBILE_CACHE_DIR" && pwd)"

# Ensure GOPATH/mobile is set up
MOBILE_DIR="$(go env GOPATH)/src/golang.org/x/mobile"
if [ ! -d "$MOBILE_DIR/bind" ]; then
    echo_error "golang.org/x/mobile/bind not found. Installing..."
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

# Build for macOS
build_macos() {
    echo_info "Building macOS XCFramework (universal)..."

    # Build for macOS arm64
    echo_info "  - Building for macOS/arm64..."
    GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 \
    gomobile bind -iosversion=13.0 \
        -tags="$BUILD_TAGS" \
        -target=macos/arm64 \
        -o "$OUTPUT_DIR/${FRAMEWORK_NAME}_macos_arm64.xcframework" \
        -ldflags='-s -w' \
        "$PACKAGE"

    # Build for macOS amd64
    echo_info "  - Building for macOS/amd64..."
    GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 \
    gomobile bind -iosversion=13.0 \
        -tags="$BUILD_TAGS" \
        -target=macos/amd64 \
        -o "$OUTPUT_DIR/${FRAMEWORK_NAME}_macos_amd64.xcframework" \
        -ldflags='-s -w' \
        "$PACKAGE"

    # Create universal XCFramework manually
    echo_info "  - Creating universal macOS XCFramework..."
    mkdir -p "$OUTPUT_DIR/${FRAMEWORK_NAME}_macOS.xcframework/macos-arm64_x86_64"

    # Copy the arm64 framework as base
    cp -R "$OUTPUT_DIR/${FRAMEWORK_NAME}_macos_arm64.xcframework/macos-arm64/${FRAMEWORK_NAME}_macos_arm64.framework" \
       "$OUTPUT_DIR/${FRAMEWORK_NAME}_macOS.xcframework/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework"

    # Rename binary
    mv "$OUTPUT_DIR/${FRAMEWORK_NAME}_macOS.xcframework/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework/Versions/A/${FRAMEWORK_NAME}_macos_arm64" \
       "$OUTPUT_DIR/${FRAMEWORK_NAME}_macOS.xcframework/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework/Versions/A/${FRAMEWORK_NAME}"

    # Update symlinks
    cd "$OUTPUT_DIR/${FRAMEWORK_NAME}_macOS.xcframework/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework"
    rm -f "${FRAMEWORK_NAME}_macos_arm64"
    ln -s "Versions/Current/${FRAMEWORK_NAME}" "${FRAMEWORK_NAME}"
    cd - > /dev/null

    # Combine binaries using lipo
    lipo -create \
        "$OUTPUT_DIR/${FRAMEWORK_NAME}_macos_arm64.xcframework/macos-arm64/${FRAMEWORK_NAME}_macos_arm64.framework/Versions/A/${FRAMEWORK_NAME}_macos_arm64" \
        "$OUTPUT_DIR/${FRAMEWORK_NAME}_macos_amd64.xcframework/macos-x86_64/${FRAMEWORK_NAME}_macos_amd64.framework/Versions/A/${FRAMEWORK_NAME}_macos_amd64" \
        -output "$OUTPUT_DIR/${FRAMEWORK_NAME}_macOS.xcframework/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework/Versions/A/${FRAMEWORK_NAME}"

    # Create Info.plist
    cat > "$OUTPUT_DIR/${FRAMEWORK_NAME}_macOS.xcframework/Info.plist" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>AvailableLibraries</key>
    <array>
        <dict>
            <key>BinaryPath</key>
            <string>${FRAMEWORK_NAME}.framework/Versions/A/${FRAMEWORK_NAME}</string>
            <key>LibraryIdentifier</key>
            <string>macos-arm64_x86_64</string>
            <key>LibraryPath</key>
            <string>${FRAMEWORK_NAME}.framework</string>
            <key>SupportedArchitectures</key>
            <array>
                <string>arm64</string>
                <string>x86_64</string>
            </array>
            <key>SupportedPlatform</key>
            <string>macos</string>
        </dict>
    </array>
    <key>CFBundlePackageType</key>
    <string>XFWK</string>
    <key>XCFrameworkFormatVersion</key>
    <string>1.0</string>
</dict>
</plist>
EOF

    # Clean up intermediate frameworks
    rm -rf "$OUTPUT_DIR/${FRAMEWORK_NAME}_macos_arm64.xcframework"
    rm -rf "$OUTPUT_DIR/${FRAMEWORK_NAME}_macos_amd64.xcframework"

    echo_info "✅ macOS XCFramework created: $OUTPUT_DIR/${FRAMEWORK_NAME}_macOS.xcframework"
}

# Build for iOS
build_ios() {
    echo_info "Building iOS XCFramework (arm64 + simulator)..."

    # Build for iOS devices (arm64)
    echo_info "  - Building for iOS/arm64..."
    GOOS=ios GOARCH=arm64 CGO_ENABLED=1 \
    gomobile bind -iosversion=13.0 \
        -tags="$BUILD_TAGS" \
        -target=ios/arm64 \
        -o "$OUTPUT_DIR/${FRAMEWORK_NAME}_ios_arm64.xcframework" \
        -ldflags='-s -w' \
        "$PACKAGE"

    # Build for iOS simulator (arm64)
    echo_info "  - Building for iOS simulator (arm64)..."
    GOOS=ios GOARCH=arm64 CGO_ENABLED=1 \
    gomobile bind -iosversion=13.0 \
        -tags="$BUILD_TAGS" \
        -target=iossimulator/arm64 \
        -o "$OUTPUT_DIR/${FRAMEWORK_NAME}_ios_simulator.xcframework" \
        -ldflags='-s -w' \
        "$PACKAGE"

    # Create universal iOS XCFramework manually
    echo_info "  - Creating universal iOS XCFramework..."
    mkdir -p "$OUTPUT_DIR/${FRAMEWORK_NAME}_iOS.xcframework"

    # Copy both framework directories
    cp -R "$OUTPUT_DIR/${FRAMEWORK_NAME}_ios_arm64.xcframework/ios-arm64" \
       "$OUTPUT_DIR/${FRAMEWORK_NAME}_iOS.xcframework/"

    cp -R "$OUTPUT_DIR/${FRAMEWORK_NAME}_ios_simulator.xcframework/ios-arm64-simulator" \
       "$OUTPUT_DIR/${FRAMEWORK_NAME}_iOS.xcframework/"

    # Normalize framework/module names so Swift uses `import Mate` on all iOS slices.
    normalize_ios_framework_slice "$OUTPUT_DIR/${FRAMEWORK_NAME}_iOS.xcframework/ios-arm64"
    normalize_ios_framework_slice "$OUTPUT_DIR/${FRAMEWORK_NAME}_iOS.xcframework/ios-arm64-simulator"

    # Create combined Info.plist
    cat > "$OUTPUT_DIR/${FRAMEWORK_NAME}_iOS.xcframework/Info.plist" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>AvailableLibraries</key>
    <array>
        <dict>
            <key>BinaryPath</key>
            <string>${FRAMEWORK_NAME}.framework/${FRAMEWORK_NAME}</string>
            <key>LibraryIdentifier</key>
            <string>ios-arm64</string>
            <key>LibraryPath</key>
            <string>${FRAMEWORK_NAME}.framework</string>
            <key>SupportedArchitectures</key>
            <array>
                <string>arm64</string>
            </array>
            <key>SupportedPlatform</key>
            <string>ios</string>
        </dict>
        <dict>
            <key>BinaryPath</key>
            <string>${FRAMEWORK_NAME}.framework/${FRAMEWORK_NAME}</string>
            <key>LibraryIdentifier</key>
            <string>ios-arm64-simulator</string>
            <key>LibraryPath</key>
            <string>${FRAMEWORK_NAME}.framework</string>
            <key>SupportedArchitectures</key>
            <array>
                <string>arm64</string>
            </array>
            <key>SupportedPlatform</key>
            <string>ios</string>
            <key>SupportedPlatformVariant</key>
            <string>simulator</string>
        </dict>
    </array>
    <key>CFBundlePackageType</key>
    <string>XFWK</string>
    <key>XCFrameworkFormatVersion</key>
    <string>1.0</string>
</dict>
</plist>
EOF

    # Clean up intermediate frameworks
    rm -rf "$OUTPUT_DIR/${FRAMEWORK_NAME}_ios_arm64.xcframework"
    rm -rf "$OUTPUT_DIR/${FRAMEWORK_NAME}_ios_simulator.xcframework"

    echo_info "✅ iOS XCFramework created: $OUTPUT_DIR/${FRAMEWORK_NAME}_iOS.xcframework"
}

# Build unified XCFramework with all platforms
build_unified() {
    echo_info "Building unified XCFramework with iOS and macOS support..."
    
    # First build all platforms
    echo_info "Step 1: Building all platforms..."
    local ios_arm64_framework="$TEMP_DIR/ios_arm64"
    local ios_sim_framework="$TEMP_DIR/ios_simulator"
    local macos_arm64_framework="$TEMP_DIR/macos_arm64"
    local macos_amd64_framework="$TEMP_DIR/macos_amd64"
    
    mkdir -p "$ios_arm64_framework" "$ios_sim_framework" "$macos_arm64_framework" "$macos_amd64_framework"
    
    # Build iOS arm64
    echo_info "  - Building for iOS/arm64..."
    GOOS=ios GOARCH=arm64 CGO_ENABLED=1 \
    gomobile bind -iosversion=13.0 \
        -tags="$BUILD_TAGS" \
        -target=ios/arm64 \
        -o "$ios_arm64_framework/${FRAMEWORK_NAME}_ios_arm64.xcframework" \
        -ldflags='-s -w' \
        "$PACKAGE"
    
    # Build iOS simulator
    echo_info "  - Building for iOS simulator/arm64..."
    GOOS=ios GOARCH=arm64 CGO_ENABLED=1 \
    gomobile bind -iosversion=13.0 \
        -tags="$BUILD_TAGS" \
        -target=iossimulator/arm64 \
        -o "$ios_sim_framework/${FRAMEWORK_NAME}_ios_simulator.xcframework" \
        -ldflags='-s -w' \
        "$PACKAGE"
    
    # Build macOS arm64
    echo_info "  - Building for macOS/arm64..."
    GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 \
    gomobile bind -iosversion=13.0 \
        -tags="$BUILD_TAGS" \
        -target=macos/arm64 \
        -o "$macos_arm64_framework/${FRAMEWORK_NAME}_macos_arm64.xcframework" \
        -ldflags='-s -w' \
        "$PACKAGE"
    
    # Build macOS amd64
    echo_info "  - Building for macOS/amd64..."
    GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 \
    gomobile bind -iosversion=13.0 \
        -tags="$BUILD_TAGS" \
        -target=macos/amd64 \
        -o "$macos_amd64_framework/${FRAMEWORK_NAME}_macos_amd64.xcframework" \
        -ldflags='-s -w' \
        "$PACKAGE"
    
    echo_info "Step 2: Merging frameworks into unified XCFramework..."
    
    # Create the unified xcframework directory structure
    local unified_path="$OUTPUT_DIR/${FRAMEWORK_NAME}.xcframework"
    mkdir -p "$unified_path"
    
    # Copy iOS architectures
    echo_info "  - Adding iOS architectures..."
    cp -R "$ios_arm64_framework/${FRAMEWORK_NAME}_ios_arm64.xcframework/ios-arm64" \
        "$unified_path/ios-arm64"
    
    cp -R "$ios_sim_framework/${FRAMEWORK_NAME}_ios_simulator.xcframework/ios-arm64-simulator" \
        "$unified_path/ios-arm64-simulator"

    # Normalize framework/module names so Swift uses `import Mate` on all iOS slices.
    normalize_ios_framework_slice "$unified_path/ios-arm64"
    normalize_ios_framework_slice "$unified_path/ios-arm64-simulator"
    
    # Copy macOS architectures and merge with lipo
    echo_info "  - Adding macOS architectures..."
    mkdir -p "$unified_path/macos-arm64_x86_64"
    
    cp -R "$macos_arm64_framework/${FRAMEWORK_NAME}_macos_arm64.xcframework/macos-arm64/${FRAMEWORK_NAME}_macos_arm64.framework" \
        "$unified_path/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework"
    
    # Update macOS framework binary name
    mv "$unified_path/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework/Versions/A/${FRAMEWORK_NAME}_macos_arm64" \
        "$unified_path/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework/Versions/A/${FRAMEWORK_NAME}_temp"
    
    # Merge macOS binaries
    lipo -create \
        "$unified_path/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework/Versions/A/${FRAMEWORK_NAME}_temp" \
        "$macos_amd64_framework/${FRAMEWORK_NAME}_macos_amd64.xcframework/macos-x86_64/${FRAMEWORK_NAME}_macos_amd64.framework/Versions/A/${FRAMEWORK_NAME}_macos_amd64" \
        -output "$unified_path/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework/Versions/A/${FRAMEWORK_NAME}"
    
    rm -f "$unified_path/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework/Versions/A/${FRAMEWORK_NAME}_temp"
    
    # Update macOS symlinks
    cd "$unified_path/macos-arm64_x86_64/${FRAMEWORK_NAME}.framework"
    rm -f "${FRAMEWORK_NAME}_macos_arm64" "${FRAMEWORK_NAME}_macos_amd64"
    ln -sf "Versions/Current/${FRAMEWORK_NAME}" "${FRAMEWORK_NAME}"
    cd - > /dev/null
    
    echo_info "Step 3: Creating unified Info.plist..."
    
    # Create comprehensive Info.plist
    cat > "$unified_path/Info.plist" << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>AvailableLibraries</key>
    <array>
        <dict>
            <key>BinaryPath</key>
            <string>Mate.framework/Mate</string>
            <key>LibraryIdentifier</key>
            <string>ios-arm64</string>
            <key>LibraryPath</key>
            <string>Mate.framework</string>
            <key>SupportedArchitectures</key>
            <array>
                <string>arm64</string>
            </array>
            <key>SupportedPlatform</key>
            <string>ios</string>
        </dict>
        <dict>
            <key>BinaryPath</key>
            <string>Mate.framework/Mate</string>
            <key>LibraryIdentifier</key>
            <string>ios-arm64-simulator</string>
            <key>LibraryPath</key>
            <string>Mate.framework</string>
            <key>SupportedArchitectures</key>
            <array>
                <string>arm64</string>
            </array>
            <key>SupportedPlatform</key>
            <string>ios</string>
            <key>SupportedPlatformVariant</key>
            <string>simulator</string>
        </dict>
        <dict>
            <key>BinaryPath</key>
            <string>Mate.framework/Versions/A/Mate</string>
            <key>LibraryIdentifier</key>
            <string>macos-arm64_x86_64</string>
            <key>LibraryPath</key>
            <string>Mate.framework</string>
            <key>SupportedArchitectures</key>
            <array>
                <string>arm64</string>
                <string>x86_64</string>
            </array>
            <key>SupportedPlatform</key>
            <string>macos</string>
        </dict>
    </array>
    <key>CFBundlePackageType</key>
    <string>XFWK</string>
    <key>XCFrameworkFormatVersion</key>
    <string>1.0</string>
</dict>
</plist>
EOF
    
    # Clean up temp directory
    echo_info "Step 4: Cleaning up intermediate files..."
    rm -rf "$TEMP_DIR"
    rm -rf "$WORK_TMP_DIR"
    
    echo_info "✅ Unified XCFramework created: $unified_path"
    echo_info "   Supported platforms:"
    echo_info "   - iOS (arm64 device + arm64 simulator)"
    echo_info "   - macOS (arm64 + x86_64)"
}

# Main build logic
case "$BUILD_TYPE" in
    ios)
        build_ios
        ;;
    macos)
        build_macos
        ;;
    all)
        build_ios
        build_macos
        ;;
    unified)
        build_unified
        ;;
    *)
        echo_error "Unknown build type: $BUILD_TYPE"
        echo "Usage: $0 [ios|macos|all|unified]"
        echo ""
        echo "Options:"
        echo "  ios      - Build iOS framework only"
        echo "  macos    - Build macOS framework only"
        echo "  all      - Build separate iOS and macOS frameworks"
        echo "  unified  - Build single XCFramework with all platforms (recommended)"
        exit 1
        ;;
esac

echo ""
echo_info "🎉 Build completed!"
echo ""
echo_info "Generated frameworks:"
ls -lh "$OUTPUT_DIR"/*.xcframework 2>/dev/null || echo_warn "  No frameworks found"
echo ""
echo_info "To use in Xcode:"
echo_info "  1. Drag the .xcframework into your project"
echo_info "  2. Add to 'Frameworks, Libraries, and Embedded Content'"
echo_info "  3. Import with: import Mate"
