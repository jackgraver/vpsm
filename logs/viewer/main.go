// Command vpsm-logs is the log half of VPSM (VPS Monitor): an interactive
// viewer for log files of any format, with a best-guess level and
// timestamp on every line and live search over them.
//
// For now it reads logs on this machine. Pulling them from each VPS through
// mini-bot, the way resources/collector pulls samples, is the next step.
//
// This file is just the entry point. All real work — flags, loading,
// parsing, the viewer — happens in startup.Run and the packages it wires
// together; keep main itself minimal.
package main

import "vpsm-logs/internal/startup"

func main() {
	startup.Run()
}
