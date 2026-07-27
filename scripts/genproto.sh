#!/usr/bin/env bash
#
# Regenerate the Meshtastic protobuf bindings in internal/meshtastic/meshpb.
#
# The generated files ARE COMMITTED. Building meshbbs must not require protoc,
# the submodule, or a network fetch — §10 puts a lot of weight on `go build`
# working on a Raspberry Pi with nothing else installed. Run this only when
# bumping the pinned protobufs revision.
#
# §4 `[D3]` decides that we vendor meshtastic/protobufs and write our own thin
# transport rather than depending on someone else's Go library. This script is
# the vendoring half of that decision.
#
# Usage: scripts/genproto.sh
set -euo pipefail

cd "$(dirname "$0")/.."

SUBMODULE=third_party/meshtastic-protobufs
OUT_PKG=github.com/aghman/meshbbs/internal/meshtastic/meshpb
# Keep in step with the google.golang.org/protobuf version in go.mod: the
# generated code asserts at init that the runtime is new enough.
PROTOC_GEN_GO_VERSION=v1.36.11

if [ ! -f "$SUBMODULE/meshtastic/mesh.proto" ]; then
  echo "The protobufs submodule is not checked out. Run:" >&2
  echo "  git submodule update --init $SUBMODULE" >&2
  exit 1
fi

if ! command -v protoc >/dev/null; then
  echo "protoc is not installed (brew install protobuf, apt install protobuf-compiler)" >&2
  exit 1
fi

# Only the transitive closure of mesh.proto, which is where ToRadio, FromRadio
# and MeshPacket live. The rest of the repo — MQTT, admin, store-and-forward,
# the phone-app-only messages — is deliberately not generated: every message we
# compile in is one more parser reachable from the radio, and §12.5 counts
# those.
PROTOS=(
  meshtastic/mesh.proto
  meshtastic/channel.proto
  meshtastic/config.proto
  meshtastic/device_ui.proto
  meshtastic/module_config.proto
  meshtastic/atak.proto
  meshtastic/portnums.proto
  meshtastic/telemetry.proto
  meshtastic/xmodem.proto
)

# The upstream files declare go_package = github.com/meshtastic/go/generated,
# an import path that does not exist as a Go module. Remap each one rather than
# patching the vendored .proto files, so the submodule stays byte-identical to
# upstream and bumping it is a plain checkout.
MAPPINGS=()
for p in "${PROTOS[@]}"; do
  MAPPINGS+=("--go_opt=M${p}=${OUT_PKG};meshpb")
done

BIN=$(mktemp -d)
trap 'rm -rf "$BIN"' EXIT
GOBIN="$BIN" go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"

rm -f internal/meshtastic/meshpb/*.pb.go
mkdir -p internal/meshtastic/meshpb

PATH="$BIN:$PATH" protoc \
  --proto_path="$SUBMODULE" \
  --go_out=. \
  --go_opt=module=github.com/aghman/meshbbs \
  "${MAPPINGS[@]}" \
  "${PROTOS[@]/#/$SUBMODULE/}"

echo "Generated from $(git -C "$SUBMODULE" describe --tags --always) with protoc $(protoc --version | awk '{print $2}')"
gofmt -l internal/meshtastic/meshpb
