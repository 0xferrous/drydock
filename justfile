# List recipes.
default:
    @just --list

# Format the flake sources.
fmt:
    nix fmt

# Build the CLI to ./result.
build:
    nix build .#drydock -o result

# Build both the CLI and the host portal.
build-all:
    nix build .#drydock -o result
    nix build .#drydock-portal -o result-portal

# End-to-end check of the lifecycle, including the failure modes. Run this
# before handing a build over. Requires KVM; takes a few minutes.
e2e:
    #!/usr/bin/env bash
    set -euo pipefail
    nix build .#drydock -o result
    d=./result/bin/drydock
    name=drydock-e2e
    state="$${XDG_STATE_HOME:-$$HOME/.local/state}/drydock/vms/$name"
    cleanup() { "$d" down --name "$name" >/dev/null 2>&1 || true; rm -rf "$state"; }
    trap cleanup EXIT

    echo "== reset (fresh disk + boot)"
    "$d" reset --name "$name" --flake ./demo --host demo
    "$d" status --name "$name" --json | jq -e '.state == "running"' >/dev/null

    echo "== up again while healthy"
    "$d" up --name "$name" | tee /tmp/dd-e2e-up
    grep -q "already running" /tmp/dd-e2e-up
    grep -q "display: http" /tmp/dd-e2e-up

    echo "== screenshot"
    "$d" screenshot --name "$name" /tmp/dd-e2e.png
    test "$(head -c4 /tmp/dd-e2e.png | od -An -tx1 | tr -d ' \n')" = "89504e47"

    echo "== apply (live switch)"
    "$d" apply --name "$name" | grep -q applied

    echo "== break the guest, then up must heal"
    "$d" ssh --name "$name" -- 'systemctl stop sshd' >/dev/null 2>&1 || true
    sleep 2
    "$d" up --name "$name" | tee /tmp/dd-e2e-heal
    grep -q "unreachable; restarting" /tmp/dd-e2e-heal
    "$d" status --name "$name" --json | jq -e '.state == "running"' >/dev/null

    echo "== down"
    "$d" down --name "$name"
    "$d" status --name "$name" --json | jq -e '.state == "stopped"' >/dev/null

    echo "== dry-run never boots"
    "$d" up --name "$name" --dry-run | grep -q "^built /nix/store"

    echo "e2e: OK"

# Run the host portal for local protocol testing.
portal:
    nix run .#drydock-portal -- --bind 127.0.0.1 --port 8787 --default-flake ./demo --default-host demo

# Boot the bundled demo desktop.
demo:
    nix run . -- up --flake ./demo --host demo

# Rebuild the demo from a clean disk.
demo-reset:
    nix run . -- reset --flake ./demo --host demo

status:
    nix run . -- status

# Stop the default VM.
down:
    nix run . -- down
