bin := "vibemon"
prefix := env_var('HOME') / ".local/bin"
plist := env_var('HOME') / "Library/LaunchAgents/com.mxcd.vibemon.plist"

# Matches the SDK the Wails objc files are built against; without it the linker warns on every build.
export MACOSX_DEPLOYMENT_TARGET := "26.0"

default:
    @just --list

build:
    go build -o {{bin}} .

test:
    go test ./...

check: test
    gofmt -l .
    go vet ./...

run: build
    ./{{bin}}

# Stop a running instance (the menu bar app holds the keychain items open).
stop:
    -pkill -x {{bin}}

# Render the tray glyph and open it — the only way to judge icon.go's geometry.
icon:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(mktemp -d)/vibemon-icon.png
    VIBEMON_ICON_OUT="$out" go test -run TestCRTIconPreview . >/dev/null
    sips -z 264 264 "$out" --out "${out%.png}-8x.png" >/dev/null
    open "${out%.png}-8x.png"

install: check build stop
    mkdir -p {{prefix}}
    cp {{bin}} {{prefix}}/{{bin}}
    @echo "installed to {{prefix}}/{{bin}}"

# Start vibemon at login and keep it alive if it crashes.
autostart: install
    mkdir -p "{{parent_directory(plist)}}"
    printf '%s\n' \
      '<?xml version="1.0" encoding="UTF-8"?>' \
      '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
      '<plist version="1.0"><dict>' \
      '  <key>Label</key><string>com.mxcd.vibemon</string>' \
      '  <key>ProgramArguments</key><array><string>{{prefix}}/{{bin}}</string></array>' \
      '  <key>RunAtLoad</key><true/>' \
      '  <key>KeepAlive</key><true/>' \
      '  <key>StandardOutPath</key><string>{{env_var("HOME")}}/Library/Logs/vibemon.log</string>' \
      '  <key>StandardErrorPath</key><string>{{env_var("HOME")}}/Library/Logs/vibemon.log</string>' \
      '</dict></plist>' > {{plist}}
    -launchctl bootout gui/$(id -u)/com.mxcd.vibemon 2>/dev/null
    launchctl bootstrap gui/$(id -u) {{plist}}
    @echo "vibemon will start at login — logs in ~/Library/Logs/vibemon.log"

no-autostart:
    -launchctl bootout gui/$(id -u)/com.mxcd.vibemon
    -rm -f {{plist}}
    @echo "autostart removed"

# Forget every stored account. Does not touch Claude Code's own credentials.
reset-vault:
    -security delete-generic-password -s vibemon-accounts
    @echo "vault cleared — run `vibemon capture` to re-add accounts"
