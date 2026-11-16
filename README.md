# VPSM (VPS Monitor)

A lightweight monitoring system for VPS hosts.

**Layout:**
- **mini-bot/** — tiny Rust agent deployed on each VPS, shared by both halves
- **resources/** — host metrics: the Go collector pulls CPU/RAM/disk/network samples from mini-bot over SSH, stores them in SQLite and shows them in the terminal
- **logs/** — log search: a Go TUI (`vpsm-logs`) that reads log files of any format and filters them by text and level

**Status:** Resource monitoring works end to end over pull-over-SSH. Log search works on local files and stdin; pulling logs from a VPS is next.

For more details, see [CLAUDE.md](CLAUDE.md).
