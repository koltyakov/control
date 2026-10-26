package system

// DXCore is a system API available on Windows 10 version 2004 and newer.
// It provides actual hardware/integrated flags, LUIDs, and 64-bit dedicated
// capacities without CGO or Win32_VideoController's truncated AdapterRAM.
// ABI reference: microsoft/DirectX-Headers include/directx/dxcore_interface.h.

import (
	"bytes"
	"context"
	"syscall"
	"unsafe"

	"github.com/koltyakov/control/internal/model"
	"golang.org/x/sys/windows"
)

var dxFactoryID = windows.GUID{Data1: 0x78ee5945, Data2: 0xc36e, Data3: 0x4b13, Data4: [8]byte{0xa6, 0x69, 0x00, 0x5d, 0xd1, 0x1c, 0x0f, 0x06}}
var dxListID = windows.GUID{Data1: 0x526c7776, Data2: 0x40e9, Data3: 0x459b, Data4: [8]byte{0xb7, 0x11, 0xf3, 0x2a, 0xd7, 0x6d, 0xfc, 0x28}}
var dxAdapterID = windows.GUID{Data1: 0xf0db4c7f, Data2: 0xfe5a, Data3: 0x42a2, Data4: [8]byte{0xbd, 0x62, 0xf2, 0xa6, 0xcf, 0x6f, 0xc8, 0x3e}}
var dxGraphics12 = windows.GUID{Data1: 0x0c9ece4d, Data2: 0x2f6e, Data3: 0x4f01, Data4: [8]byte{0x8c, 0x96, 0xe8, 0x9e, 0x33, 0x1b, 0x47, 0xb1}}
var dxGraphics11 = windows.GUID{Data1: 0x8c47866b, Data2: 0x7583, Data3: 0x450d, Data4: [8]byte{0xf0, 0xf0, 0x6b, 0xad, 0xa8, 0x95, 0xaf, 0x4b}}
var dxCompute = windows.GUID{Data1: 0x248e2800, Data2: 0xa793, Data3: 0x4724, Data4: [8]byte{0xab, 0xaa, 0x23, 0xa6, 0xde, 0x1b, 0xe0, 0x90}}
var dxNPU = windows.GUID{Data1: 0xd46140c4, Data2: 0xadd7, Data3: 0x451b, Data4: [8]byte{0x9e, 0x56, 0x06, 0xfe, 0x8c, 0x3b, 0x58, 0xed}}
var dxGPU = windows.GUID{Data1: 0xb69eb219, Data2: 0x3ded, Data3: 0x4464, Data4: [8]byte{0x97, 0x9f, 0xa0, 0x0b, 0xd4, 0x68, 0x70, 0x06}}

var dxCreateFactory = windows.NewLazySystemDLL("dxcore.dll").NewProc("DXCoreCreateAdapterFactory")

// The first three vtable slots are IUnknown. Each method below uses the
// documented IDXCoreAdapterFactory/List/Adapter slots directly.
type dxObject struct{ VTable *[13]uintptr }

func dxRelease(object *dxObject) {
	if object != nil {
		_, _, _ = syscall.SyscallN(object.VTable[2], uintptr(unsafe.Pointer(object)))
	}
}

func dxCoreInventory(ctx context.Context) []gpuDevice {
	if dxCreateFactory.Find() != nil {
		return nil
	}
	var factory *dxObject
	result, _, _ := dxCreateFactory.Call(uintptr(unsafe.Pointer(&dxFactoryID)), uintptr(unsafe.Pointer(&factory)))
	defer dxRelease(factory)
	if int32(result) < 0 || factory == nil {
		return nil
	}
	var devices []gpuDevice
	seen := map[windows.LUID]bool{}
	for _, attribute := range []*windows.GUID{&dxGraphics12, &dxGraphics11, &dxCompute, &dxNPU} {
		devices = appendDXAdapters(ctx, factory, attribute, devices, seen)
	}
	return devices
}

func appendDXAdapters(ctx context.Context, factory *dxObject, attribute *windows.GUID, devices []gpuDevice, seen map[windows.LUID]bool) []gpuDevice {
	var list *dxObject
	result, _, _ := syscall.SyscallN(factory.VTable[3], uintptr(unsafe.Pointer(factory)), 1, uintptr(unsafe.Pointer(attribute)), uintptr(unsafe.Pointer(&dxListID)), uintptr(unsafe.Pointer(&list)))
	defer dxRelease(list)
	if int32(result) < 0 || list == nil {
		return devices
	}
	count, _, _ := syscall.SyscallN(list.VTable[4], uintptr(unsafe.Pointer(list)))
	for index := uintptr(0); index < min(count, 64) && len(devices) < model.MaxGPUs && ctx.Err() == nil; index++ {
		var adapter *dxObject
		result, _, _ = syscall.SyscallN(list.VTable[3], uintptr(unsafe.Pointer(list)), index, uintptr(unsafe.Pointer(&dxAdapterID)), uintptr(unsafe.Pointer(&adapter)))
		if int32(result) >= 0 && adapter != nil {
			device := dxDevice(adapter)
			if device.native && device.info.Valid() && !seen[device.luid] {
				devices = append(devices, device)
				seen[device.luid] = true
			}
		}
		dxRelease(adapter)
	}
	return devices
}

func dxProperty[T any](adapter *dxObject, property uint32, out *T) bool {
	supported, _, _ := syscall.SyscallN(adapter.VTable[5], uintptr(unsafe.Pointer(adapter)), uintptr(property))
	if uint8(supported) == 0 {
		return false
	}
	result, _, _ := syscall.SyscallN(adapter.VTable[6], uintptr(unsafe.Pointer(adapter)), uintptr(property), unsafe.Sizeof(*out), uintptr(unsafe.Pointer(out)))
	return int32(result) >= 0
}

func dxAttribute(adapter *dxObject, attribute *windows.GUID) bool {
	result, _, _ := syscall.SyscallN(adapter.VTable[4], uintptr(unsafe.Pointer(adapter)), uintptr(unsafe.Pointer(attribute)))
	return uint8(result) != 0
}

func dxDescription(adapter *dxObject) string {
	var size uintptr
	result, _, _ := syscall.SyscallN(adapter.VTable[7], uintptr(unsafe.Pointer(adapter)), 2, uintptr(unsafe.Pointer(&size)))
	if int32(result) < 0 || size == 0 || size > 4096 {
		return ""
	}
	data := make([]byte, size)
	result, _, _ = syscall.SyscallN(adapter.VTable[6], uintptr(unsafe.Pointer(adapter)), 2, size, uintptr(unsafe.Pointer(&data[0])))
	if int32(result) < 0 {
		return ""
	}
	if end := bytes.IndexByte(data, 0); end >= 0 {
		data = data[:end]
	}
	return gpuText(string(data), 128)
}

func dxDevice(adapter *dxObject) gpuDevice {
	device := gpuDevice{}
	device.native = dxProperty(adapter, 0, &device.luid) // InstanceLuid.
	info := model.GPUInfo{Name: dxDescription(adapter)}
	var hardwareID struct{ Vendor, Device, Subsystem, Revision uint32 }
	if dxProperty(adapter, 3, &hardwareID) {
		info.Vendor = map[uint32]string{0x10de: "NVIDIA", 0x1002: "AMD", 0x1022: "AMD", 0x8086: "Intel", 0x17cb: "Qualcomm", 0x5143: "Qualcomm"}[hardwareID.Vendor]
	}
	info.Kind = gpuKind(info.Name, info.Vendor)
	var hardware, integrated uint8 // C++ bool is exactly one byte.
	flags := gpuHardwareType{GPU: dxAttribute(adapter, &dxGPU) || dxAttribute(adapter, &dxGraphics12) || dxAttribute(adapter, &dxGraphics11), NPU: dxAttribute(adapter, &dxNPU)}
	if dxProperty(adapter, 11, &hardware) {
		flags.Hardware = new(hardware != 0)
	}
	if dxProperty(adapter, 12, &integrated) {
		flags.Integrated = new(integrated != 0)
	}
	info.Kind = gpuHardwareKind(info.Kind, flags)
	if info.Displayable() {
		dxProperty(adapter, 7, &info.MemoryTotalBytes) // DedicatedAdapterMemory.
	}
	device.info = info
	return device
}
