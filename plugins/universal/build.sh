#!/bin/sh
# Builds build/vecta.jar (Java 8 bytecode, no dependencies) and runs the self-test.
# Needs a JDK 11+ on PATH; without one it runs itself in the eclipse-temurin image.
set -eu
cd "$(dirname "$0")"
VERSION=${VERSION:-0.1.0}

if ! command -v javac >/dev/null 2>&1; then
  exec docker run --rm --label vecta-build -v "$PWD":/src -w /src -e VERSION="$VERSION" \
    eclipse-temurin:21-jdk sh build.sh
fi

rm -rf build
mkdir -p build/classes build/test
javac --release 8 -Xlint:-options -encoding UTF-8 -d build/classes $(find src/main/java -name '*.java')
if [ -d src/main/resources ]; then
  cp -r src/main/resources/. build/classes/
fi
cat > build/MANIFEST.MF <<EOF
Main-Class: group.senger.vecta.universal.Launcher
Premain-Class: group.senger.vecta.universal.Agent
Launcher-Agent-Class: group.senger.vecta.universal.Agent
Implementation-Title: vecta
Implementation-Version: $VERSION
EOF
jar cfm build/vecta.jar build/MANIFEST.MF -C build/classes .

javac --release 8 -Xlint:-options -encoding UTF-8 -cp build/classes -d build/test $(find src/test/java -name '*.java')
java -cp build/classes:build/test group.senger.vecta.universal.SelfTest
ls -l build/vecta.jar
