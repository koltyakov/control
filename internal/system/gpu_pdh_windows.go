package system

// PDH reads the same "GPU Adapter Memory" counter set exposed by
// Win32_PerfFormattedData_GPUPerformanceCounters_GPUAdapterMemory, in process
// and without starting PowerShell or WMI for every sample. A query stays open
// between samples; wildcard instances are re-enumerated on each collection.

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	pdhCounterPath        = `\GPU Adapter Memory(*)\Dedicated Usage`
	pdhFormatLarge        = 0x00000400
	pdhMoreData           = 0x800007d2
	pdhStatusValidData    = 0x00000000
	pdhStatusNewData      = 0x00000001
	pdhMaxCounterInstance = 1024
)

var (
	pdhDLL                    = windows.NewLazySystemDLL("pdh.dll")
	pdhOpenQuery              = pdhDLL.NewProc("PdhOpenQueryW")
	pdhAddEnglishCounter      = pdhDLL.NewProc("PdhAddEnglishCounterW")
	pdhCollectQueryData       = pdhDLL.NewProc("PdhCollectQueryData")
	pdhGetFormattedCounterArr = pdhDLL.NewProc("PdhGetFormattedCounterArrayW")
	pdhCloseQuery             = pdhDLL.NewProc("PdhCloseQuery")
)

// PDH_FMT_COUNTERVALUE_ITEM_W on 64-bit Windows: a name pointer followed by
// PDH_FMT_COUNTERVALUE, whose 8-byte union is aligned after CStatus.
type pdhCounterItem struct {
	name   *uint16
	status uint32
	_      uint32
	value  int64
}

// Control ships only 64-bit Windows builds. Reject any other layout rather
// than reading a mismatched buffer.
const pdhItemLayoutOK = unsafe.Sizeof(pdhCounterItem{}) == 24 && unsafe.Offsetof(pdhCounterItem{}.value) == 16

type pdhGPUMemory struct {
	mu      sync.Mutex
	query   uintptr
	counter uintptr
}

// One machine-wide query serves every collector in the process.
var gpuMemoryCounters pdhGPUMemory

func (p *pdhGPUMemory) sample() (map[string]uint64, error) {
	if !pdhItemLayoutOK {
		return nil, errors.New("unsupported PDH counter layout")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.query == 0 {
		if err := p.open(); err != nil {
			return nil, err
		}
	}
	rows, err := p.collect()
	if err != nil {
		// Reopen on the next sample, for example after a driver or counter
		// provider restart. The caller may fall back for this sample.
		p.closeLocked()
		return nil, err
	}
	return windowsGPUMemory(rows), nil
}

func (p *pdhGPUMemory) open() error {
	if err := pdhOpenQuery.Find(); err != nil {
		return err
	}
	var query, counter uintptr
	if status, _, _ := pdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&query))); status != 0 {
		return pdhError("open query", status)
	}
	path, err := windows.UTF16PtrFromString(pdhCounterPath)
	if err != nil {
		_, _, _ = pdhCloseQuery.Call(query)
		return err
	}
	if status, _, _ := pdhAddEnglishCounter.Call(query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&counter))); status != 0 {
		_, _, _ = pdhCloseQuery.Call(query)
		return pdhError("add GPU memory counter", status)
	}
	p.query, p.counter = query, counter
	return nil
}

func (p *pdhGPUMemory) collect() ([]gpuMemoryRow, error) {
	if status, _, _ := pdhCollectQueryData.Call(p.query); status != 0 {
		return nil, pdhError("collect GPU memory counter", status)
	}
	// Instances can appear between the size query and the read. Retry a few
	// times with the newly reported size, and bound the allocation.
	var size, count uint32
	for range 3 {
		var buffer []uint64
		var items uintptr
		if size > 0 {
			buffer = make([]uint64, (size+7)/8)
			items = uintptr(unsafe.Pointer(&buffer[0]))
		}
		status, _, _ := pdhGetFormattedCounterArr.Call(p.counter, pdhFormatLarge, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), items)
		if uint32(status) == pdhMoreData {
			if size == 0 || size > pdhMaxCounterInstance*1024 {
				return nil, errors.New("GPU memory counter buffer is too large")
			}
			continue
		}
		if status != 0 {
			return nil, pdhError("read GPU memory counter", status)
		}
		if count > pdhMaxCounterInstance || uintptr(count)*unsafe.Sizeof(pdhCounterItem{}) > uintptr(len(buffer))*8 {
			return nil, errors.New("GPU memory counter returned an invalid item count")
		}
		rows := make([]gpuMemoryRow, 0, count)
		if count > 0 {
			for _, item := range unsafe.Slice((*pdhCounterItem)(unsafe.Pointer(&buffer[0])), count) {
				row := gpuMemoryRow{Name: windows.UTF16PtrToString(item.name)}
				if (item.status == pdhStatusValidData || item.status == pdhStatusNewData) && item.value >= 0 {
					value := uint64(item.value)
					row.DedicatedUsage = &value
				}
				rows = append(rows, row)
			}
		}
		// Item names point into the buffer.
		runtime.KeepAlive(buffer)
		return rows, nil
	}
	return nil, errors.New("GPU memory counter instances kept changing")
}

func (p *pdhGPUMemory) closeLocked() {
	if p.query != 0 {
		_, _, _ = pdhCloseQuery.Call(p.query)
	}
	p.query, p.counter = 0, 0
}

func pdhError(operation string, status uintptr) error {
	return fmt.Errorf("PDH %s: status 0x%08x", operation, uint32(status))
}
