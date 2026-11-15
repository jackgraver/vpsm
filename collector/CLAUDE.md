# collector

The VPSM central server. Receives usage reports sent by every `mini-bot`
agent (see `../mini-bot/CLAUDE.md`), and is where that data gets
aggregated/stored for review.

## Layout

- `main.go` — fixed entry point: a doc comment describing the app, then
  `func main() { startup.Run() }`. Don't add logic here.
- `internal/startup/startup.go` — `startup.Run()` is where real wiring
  happens: load config, set up storage, start the HTTP server and routes.

Add new packages under `internal/` as pieces are built out (e.g. a `storage`
package, an `api` package for handlers) rather than growing `startup.go`
indefinitely.

## Toolchain

Go 1.25, module `vpsm-collector`. No storage backend or API shape chosen
yet — decide those alongside the mini-bot<->collector wire protocol (see
root `CLAUDE.md`) so both ends agree.
