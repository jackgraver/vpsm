// Package startup wires up the collector: configuration, storage, and the
// HTTP server that receives reports from mini-bot agents.
package startup

import "log"

func Run() {
	// TODO: load config (listen address, storage backend)
	// TODO: initialize storage
	// TODO: start HTTP server and register routes
	log.Println("vpsm-collector starting up...")
}
