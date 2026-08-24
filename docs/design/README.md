# Product and UI contracts

These documents define behavior that maintainers must preserve. They are not
implementation handoff notes or screenshots of a temporary design exercise.

| Contract | Purpose |
|---|---|
| [Web UI](web-ui.md) | Information architecture, Docker terminology, action placement, Compose no-build policy, and screen states. |
| [Web UI acceptance](web-ui-acceptance.md) | Human and automated checks for interaction, accessibility, responsive layout, and live VM behavior. |
| [Container stats](live-metrics.md) | Viewer-scoped metrics transport, freshness, aggregation, and resource limits. |

When a product decision changes, update the relevant contract in the same pull
request as the implementation and its acceptance coverage. Completed gap
reports, review prompts, and implementation handoff notes do not belong here.
