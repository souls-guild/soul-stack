//go:build !linux && !darwin

package utilization

import "context"

// systemSource вне Linux/macOS (windows и пр.) — заглушка (ADR-072:
// production-target — Linux). Все методы zero-value.
type systemSource struct{}

// NewSystemSource — заглушка не-Linux (см. doc systemSource).
func NewSystemSource() Source { return systemSource{} }

func (systemSource) Load(context.Context) LoadAvg        { return LoadAvg{} }
func (systemSource) Memory(context.Context) MemInfo      { return MemInfo{} }
func (systemSource) Disks(context.Context) []Disk        { return nil }
func (systemSource) Uptime(context.Context) int64        { return 0 }
func (systemSource) CPUSample(context.Context) CPUSample { return CPUSample{} }
