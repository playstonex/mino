#!/bin/bash

# Build script for MihomoMate XCFramework
# Usage: ./build_xframework.sh [ios|macos|all]

set -e

# Configuration
PACKAGE="github.com/metacubex/mihomo/mate"
FRAMEWORK_NAME="MihomoMate"
OUTPUT_DIR="./build/xframework"
BUILD_TYPE="${1:-all}"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

echo_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
echo_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
echo_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Clean output directory
echo_info "Cleaning build directory..."
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

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
        -target=macos/arm64 \
        -o "$OUTPUT_DIR/${FRAMEWORK_NAME}_macos_arm64.xcframework" \
        -ldflags='-s -w' \
        "$PACKAGE"

    # Build for macOS amd64
    echo_info "  - Building for macOS/amd64..."
    GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 \
    gomobile bind -iosversion=13.0 \
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
        -target=ios/arm64 \
        -o "$OUTPUT_DIR/${FRAMEWORK_NAME}_ios_arm64.xcframework" \
        -ldflags='-s -w' \
        "$PACKAGE"

    # Build for iOS simulator (arm64)
    echo_info "  - Building for iOS simulator (arm64)..."
    GOOS=ios GOARCH=arm64 CGO_ENABLED=1 \
    gomobile bind -iosversion=13.0 \
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
            <string>${FRAMEWORK_NAME}_ios_arm64.framework/${FRAMEWORK_NAME}_ios_arm64</string>
            <key>LibraryIdentifier</key>
            <string>ios-arm64</string>
            <key>LibraryPath</key>
            <string>${FRAMEWORK_NAME}_ios_arm64.framework</string>
            <key>SupportedArchitectures</key>
            <array>
                <string>arm64</string>
            </array>
            <key>SupportedPlatform</key>
            <string>ios</string>
        </dict>
        <dict>
            <key>BinaryPath</key>
            <string>${FRAMEWORK_NAME}_ios_simulator.framework/${FRAMEWORK_NAME}_ios_simulator</string>
            <key>LibraryIdentifier</key>
            <string>ios-arm64-simulator</string>
            <key>LibraryPath</key>
            <string>${FRAMEWORK_NAME}_ios_simulator.framework</string>
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
    *)
        echo_error "Unknown build type: $BUILD_TYPE"
        echo "Usage: $0 [ios|macos|all]"
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
echo_info "  3. Import with: import MihomoMate"
