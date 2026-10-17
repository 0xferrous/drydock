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
