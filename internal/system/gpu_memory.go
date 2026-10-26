package system

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

func windowsGPUKey(high, low uint32) string {
	return fmt.Sprintf("luid_0x%08x_0x%08x", high, low)
}

// Counter instances identify the native adapter by LUID, not by display order
// or model name. DedicatedUsage is the adapter-wide resident memory gauge.
func parseWindowsGPUMemory(data []byte) map[string]uint64 {
	var rows []struct {
		Name           string
		DedicatedUsage *uint64
	}
	if json.Unmarshal(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}), &rows) != nil {
		return nil
	}
	result := map[string]uint64{}
	invalid := map[string]bool{}
	for _, row := range rows {
		key, physical, ok := strings.Cut(strings.ToLower(row.Name), "_phys_")
		index, err := strconv.ParseUint(physical, 10, 32)
		if !ok || err != nil || index >= 16 || row.DedicatedUsage == nil || invalid[key] {
			continue
		}
		if *row.DedicatedUsage > math.MaxUint64-result[key] {
			delete(result, key)
			invalid[key] = true
			continue
		}
		result[key] += *row.DedicatedUsage
	}
	return result
}
