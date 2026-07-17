//go:build !linux && !darwin

package utilization

import "context"

// systemSource outside Linux/macOS (windows and others) — a stub (ADR-072:
// production-target is Linux). All methods are zero-value.
type systemSource struct{}

// NewSystemSource — non-Linux stub (see doc systemSource).
func NewSystemSource() Source { return systemSource{} }

func (systemSource) Load(context.Context) LoadAvg        { return LoadAvg{} }
func (systemSource) Memory(context.Context) MemInfo      { return MemInfo{} }
func (systemSource) Disks(context.Context) []Disk        { return nil }
func (systemSource) Uptime(context.Context) int64        { return 0 }
func (systemSource) CPUSample(context.Context) CPUSample { return CPUSample{} }
