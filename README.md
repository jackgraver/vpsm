# VPSM (VPS Monitor)

A lightweight monitoring system for tracking resource usage across VPS hosts.

**Components:**
- **mini-bot** — tiny Rust agent deployed on each VPS, samples CPU/RAM/disk/network
- **collector** — Go server that receives reports, stores data, and provides query/view endpoints

**Status:** Collector built out with SQLite storage and terminal views. Agent samples complete, protocol finalized in `AGENT-CONTRACT.md`. Agent reporting coming next.

For more details, see [CLAUDE.md](CLAUDE.md).
