# Official transport prototype completion

Completed on 2026-08-15 with 78 non-empty `acceptance.json` checkpoints.

- Recommendation: `REVERSE_GRPC`
- Candidate A (Reverse gRPC): passed all 13 single-connection groups
- Candidate B (WebSocket): failed `loopback/scenario-3/scale` (`workload` passed 1/3 trials)
- Two-connection fallback required: `false`
- Final reports: `final-report.json`, `final-report.md`, and `decision-memo.md`
- Compose reality check: `compose-smoke/summary.json` (`complete=true`, `acceptance_input=false`)
- Control binary SHA-256: `b4d9ea567b9c77a738acab4697488e6b16af64745b6d779b6d5ddd608a56932b`

See `ENVIRONMENT-EXCEPTION.md` for the accepted WSL kernel-update exception.
