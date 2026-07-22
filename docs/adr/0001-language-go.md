# ADR-001. Implementation Language — Go

- **Context.** We need a language for the system's binaries that can be built easily into static artifacts, has mature SDKs for Vault, OpenTelemetry, gRPC, MCP, k8s, while still enabling a fast MVP.
- **Decision.** Go.
- **Rationale.** A ready-made ecosystem covering the entire required stack, static compilation, simple distribution of the Soul agent, low entry barrier for contributors.
- **Trade-off.** Higher GC and runtime overhead than Rust; on edge hosts with tight memory limits, Soul will be larger. We accept this as the price for delivery speed and library maturity.
