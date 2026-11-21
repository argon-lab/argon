package wal

// Monitor remains so that code which stops walcli.Services.Monitor keeps
// compiling. Argon no longer runs a background monitor: the counters it
// reported were never recorded by any operation.
type Monitor struct{}

// Stop does nothing.
func (m *Monitor) Stop() {}
