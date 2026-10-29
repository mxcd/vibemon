bin := "vibemon"
prefix := env_var('HOME') / ".local/bin"
plist := env_var('HOME') / "Library/LaunchAgents/com.mxcd.vibemon.plist"
unit := config_directory() / "systemd/user/vibemon.service"

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
[macos]
icon:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(mktemp -d)/vibemon-icon.png
    VIBEMON_ICON_OUT="$out" go test -run TestCRTIconPreview . >/dev/null
    sips -z 264 264 "$out" --out "${out%.png}-8x.png" >/dev/null
    open "${out%.png}-8x.png"

# Render the dashboard with seeded accounts and a day of history, and open it in the browser.
[macos]
web-preview:
    #!/usr/bin/env bash
    set -euo pipefail
    dir=$(mktemp -d)
    cp web/htmx.min.js "$dir/"
    VIBEMON_STATE="$dir/state" VIBEMON_WEB_OUT="$dir/index.html" go test -run TestWebPreview . >/dev/null
    open "$dir/index.html"

# Swap the binary in atomically, then restart. Stopping first raced KeepAlive: launchd relaunched the
# old binary before the copy landed.
[macos]
install: check build
    mkdir -p {{prefix}}
    cp {{bin}} {{prefix}}/.{{bin}}.new
    mv -f {{prefix}}/.{{bin}}.new {{prefix}}/{{bin}}
    launchctl kickstart -k gui/$(id -u)/com.mxcd.vibemon 2>/dev/null || pkill -x {{bin}} || true
    @echo "installed to {{prefix}}/{{bin}}"

# Start vibemon at login and keep it alive if it crashes.
[macos]
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

[macos]
no-autostart:
    -launchctl bootout gui/$(id -u)/com.mxcd.vibemon
    -rm -f {{plist}}
    @echo "autostart removed"

# Forget every stored account. Does not touch Claude Code's own credentials.
[macos]
reset-vault:
    -security delete-generic-password -s vibemon-accounts
    @echo 'vault cleared: run vibemon capture to re-add accounts'

# --- Linux: the same recipes for a headless box. A bare vibemon runs the proxies, the usage monitor
# and the dashboard in the foreground; systemd --user keeps it alive (with linger, from boot on).

# Swap the binary in atomically, then restart the unit if it is running.
[linux]
install: check build
    mkdir -p {{prefix}}
    cp {{bin}} {{prefix}}/.{{bin}}.new
    mv -f {{prefix}}/.{{bin}}.new {{prefix}}/{{bin}}
    -systemctl --user try-restart vibemon.service
    @echo "installed to {{prefix}}/{{bin}}"

# Run vibemon as a systemd user unit, restarted whenever it exits; logs go to the journal.
[linux]
autostart: install
    mkdir -p "{{parent_directory(unit)}}"
    printf '%s\n' \
      '[Unit]' \
      'Description=vibemon: Claude Code auth proxies, usage monitor and dashboard' \
      '' \
      '[Service]' \
      'ExecStart={{prefix}}/{{bin}}' \
      'Restart=always' \
      'RestartSec=5' \
      '' \
      '[Install]' \
      'WantedBy=default.target' > "{{unit}}"
    systemctl --user daemon-reload
    systemctl --user enable --now vibemon.service
    @echo "vibemon runs as a systemd user unit: journalctl --user -u vibemon -f"

[linux]
no-autostart:
    -systemctl --user disable --now vibemon.service
    -rm -f "{{unit}}"
    -systemctl --user daemon-reload
    @echo "autostart removed"

# Forget every stored account (the vault file). Claude Code's own credentials file is not touched.
[linux]
reset-vault:
    rm -f "{{env('VIBEMON_STATE', config_directory() / 'vibemon')}}/vault.json"
    @echo 'vault cleared: run vibemon capture to re-add accounts'
