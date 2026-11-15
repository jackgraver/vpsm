// Command vpsm-collector is the central server for VPSM (VPS Monitor).
//
// It receives periodic usage reports (CPU, memory, disk, network, etc.)
// sent by the mini-bot agents running on each monitored VPS, aggregates
// them, and exposes that data for review.
//
// This file is just the entry point. All real work — config, storage, the
// HTTP server and its routes — happens in startup.Run and the packages it
// wires together; keep main itself minimal.
package main

import "vpsm-collector/internal/startup"

func main() {
	startup.Run()
}
