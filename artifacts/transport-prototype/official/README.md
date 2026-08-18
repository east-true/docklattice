# Official transport prototype evidence

This directory is split across two retention channels:

- Normal Git contains this file, the completion and environment-exception
  notes, the final JSON/Markdown report, the decision memo, and the release
  asset checksum.
- The complete directory, including per-trial JSONL/log/config evidence and
  the preserved control executable, is packaged as one immutable GitHub
  Release asset. It is intentionally excluded from normal Git history.

Create the release bundle with:

```sh
./prototype/package-official-artifacts.sh
```

The command writes the archive under `artifacts/transport-prototype/release/`
and records its SHA-256 in `RELEASE-ASSET.sha256`. Upload both the archive and
the checksum file to the release. A consumer must verify the checksum before
extracting the archive.

The raw evidence contains machine-specific environment metadata. Review the
release visibility before publishing it outside a trusted project.
