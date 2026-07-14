# Bleephub runner integration against Sockerless

This harness verifies the official GitHub Actions runner against Bleephub while
its Docker executor targets a real Sockerless backend and simulator. Bleephub
owns the runner protocol and assertions; Sockerless remains the external cloud
execution substrate used by this integration test.

Run it from a Sockerless checkout that provides the backend and simulator
binaries described by `run-integration.sh`.
