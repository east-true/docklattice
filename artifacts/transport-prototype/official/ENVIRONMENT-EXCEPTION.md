# Accepted environment exception

The official matrix started on WSL kernel `6.6.87.2-microsoft-standard-WSL2`.
A WSL update restarted the environment before the final three WebSocket trials,
which then ran on `6.18.33.2-microsoft-standard-WSL2`.

Appendix A.7 specifies one identical kernel, so this is a disclosed control
exception. A clean full-matrix rerun on the new kernel was started, then stopped
at the user's direction before any trial completed. The existing 78-trial
matrix was accepted instead.

The decision-driving evidence predates the update: Reverse gRPC had already
passed all 13 aggregate groups, and WebSocket Scenario 3 scale had already
failed the `workload` check in trials 1 and 2. The post-update trial 3 passed,
leaving that WebSocket group at 1/3 passes and the recommendation unchanged.
