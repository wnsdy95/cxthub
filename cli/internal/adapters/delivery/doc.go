// Package delivery contains the driving adapters for the cxt client.
//
// It provides two client drivers as sub-packages for calling inbound ports:
//   - cli/   : command driver (cxt setup, cxt push, cxt doctor, ...)
//   - hook/  : handler of the agent tools' hook events (cxt hook --provider X --event Y)
//
// The REST/JSON HTTP server (serve) is not responsible for client duties → it is handled by a separate backend module.
package delivery
