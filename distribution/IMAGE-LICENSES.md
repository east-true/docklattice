# Container image notices

The Dockpilot Agent image contains these explicitly bundled programs:

| Component | Pinned release | License | Bundled text and upstream source |
|---|---:|---|---|
| Dockpilot | image label `org.opencontainers.image.version` | Apache-2.0 | `/licenses/Dockpilot/` |
| Docker CLI | 29.7.2 | Apache-2.0 | `/licenses/docker-cli/`; [`v29.7.2` source](https://github.com/docker/cli/tree/v29.7.2) |
| Docker Compose | 5.5.0 | Apache-2.0 | `/licenses/docker-compose/`; [`v5.5.0` source](https://github.com/docker/compose/tree/v5.5.0) |

Docker CLI is copied from the multi-platform Docker Official Image locked by
index digest and packaging-source revision in `distribution/versions.env`.
Compose is downloaded from the
official GitHub release and verified against the architecture-specific SHA-256
in that file. The corresponding upstream LICENSE and NOTICE files are fetched
from the pinned source tags with independently pinned SHA-256 values.

The final image is based on Alpine Linux 3.24.1, also locked by OCI index
digest. The image SBOM is the authoritative inventory for Alpine base packages;
their license metadata remains available through Alpine's package database in
the image. Release automation must publish that SBOM alongside this notice.
