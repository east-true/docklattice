# Transport decision memo

Decision: **REVERSE_GRPC**

only Candidate A passed every single-connection acceptance group.

Two-connection fallback trigger: **false**. When false, the fallback was not applied because at least one single-connection candidate passed; when true, this memo is interim until the required fallback experiment completes.

The comparison used the frozen thresholds from architecture Appendix A.9. Missing data and failed assertions were not converted into passes; any OOM in the three repetitions fails its group. The two-connection fallback is run only when both single-connection candidates fail, as required by A.11.

When both candidates pass, A.10 priority 2 overrides priority 1: grpc-go owns substantially more of the correctness-critical flow-control and cancellation machinery, so Candidate A is preferred even if Candidate B has fewer dependency modules or source lines. Netem performance remains an observed criterion in the final report.

ADR update target after a final selection: §5.1 transport choice and, only if required, §5.3 fallback activation. Prototype-only workload, load-driver, stub queue/store, and the losing adapter remain disposal targets under A.13.
