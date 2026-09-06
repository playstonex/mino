#!/bin/bash

DEST="../linklink/Violet/library/Mate.xcframework"
if [ ! -d "../linklink/Violet/library" ] && [ -d "../../Violet/library" ]; then
    DEST="../../Violet/library/Mate.xcframework"
fi

rm -rf "$DEST" && cp -R build/xframework/Mate.xcframework "$DEST"
