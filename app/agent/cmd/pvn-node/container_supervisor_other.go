//go:build !linux

package main

// Container supervision is Linux-only. Other platforms retain their existing
// service/update lifecycle and do not publish container readiness files.
func runContainerSupervisor() bool { return false }
func containerRuntimeDir() string  { return "" }
func signalContainerReady()        {}
