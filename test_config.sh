#!/bin/bash

echo "Testing Mihomo configuration..."

# Test 1: Check if config file is valid
echo "1. Validating configuration syntax..."
if command -v mihomo >/dev/null 2>&1; then
    mihomo -t -f bin/config.yaml
    if [ $? -eq 0 ]; then
        echo "✅ Configuration syntax is valid"
    else
        echo "❌ Configuration syntax error"
        exit 1
    fi
else
    echo "⚠️  Mihomo binary not found, skipping syntax check"
fi

# Test 2: Check server connectivity
echo "2. Testing server connectivity..."
echo "Testing Trojan server (ec2.nsfw.ink:29443)..."
if nc -z -w5 ec2.nsfw.ink 29443; then
    echo "✅ Trojan server is reachable"
else
    echo "❌ Trojan server is not reachable"
fi

echo "Testing Hysteria2 server (ec2.nsfw.ink:8991)..."
if nc -z -w5 ec2.nsfw.ink 8991; then
    echo "✅ Hysteria2 server is reachable"
else
    echo "❌ Hysteria2 server is not reachable"
fi

# Test 3: DNS resolution
echo "3. Testing DNS resolution..."
if nslookup ec2.nsfw.ink >/dev/null 2>&1; then
    echo "✅ DNS resolution works"
else
    echo "❌ DNS resolution failed"
fi

echo "Configuration test completed."