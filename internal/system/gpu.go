package system

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/processutil"
)

type gpuCollector struct {
	initialized bool
	devices     []gpuDevice
	nvidia      string
}

func (g *gpuCollector) sample(ctx context.Context) []model.GPUInfo {
	if !g.initialized {
		g.devices = gpuInventory(ctx)
		g.nvidia, _ = exec.LookPath("nvidia-smi")
		g.initialized = true
	}
	var nvidia []model.GPUInfo
	if g.nvidia != "" {
		if data, err := gpuCommand(ctx, g.nvidia, "--query-gpu=name,utilization.gpu,memory.total,memory.used", "--format=csv,noheader,nounits"); err == nil {
			nvidia = parseNVIDIAGPUs(data)
		}
	}
	var remaining []gpuDevice
	for _, device := range g.devices {
		if len(nvidia) > 0 && device.info.Vendor == "NVIDIA" && device.info.Kind != model.GPUNPU {
			for i := range nvidia {
				if nvidia[i].Name == device.info.Name && device.info.Kind != "" {
					nvidia[i].Kind = device.info.Kind
				}
			}
			continue
		}
		remaining = append(remaining, device)
	}
	result := append([]model.GPUInfo(nil), nvidia...)
	for _, info := range sampleGPUDevices(ctx, remaining) {
		if len(result) == model.MaxGPUs {
			break
		}
		if info.Valid() {
			result = append(result, info)
		}
	}
	return result
}

// Only recognizable model families are classified without a driver-provided
// hardware type. VRAM size and vendor alone do not prove a GPU is discrete.
var discreteGPUModel = regexp.MustCompile(`(?i)\b(geforce|quadro|tesla|nvs|rtx|gtx)\b|\bradeon (rx\s*\d|pro [wv]\d)|\barc(?:\(tm\))?\s+[ab]\d{3}\b|\bnvidia\s+[abhltpv]\d{1,3}\b`)
var integratedGPUModel = regexp.MustCompile(`(?i)\b(uhd|iris|hd graphics|radeon(?:\(tm\))? graphics|radeon(?:\(tm\))? \d{3}m|arc(?:\(tm\))?\s+\d{3}[vt]|tegra|jetson|orin|thor)\b`)
var npuModel = regexp.MustCompile(`(?i)\b(npu|neural|ai boost|ryzen ai|hexagon)\b`)

func gpuKind(name, vendor string) string {
	lower := strings.ToLower(name)
	for _, virtual := range []string{"virtual", "vmware", "parsec", "remote", "displaylink", "usb mobile", "microsoft basic", "software", "warp"} {
		if strings.Contains(lower, virtual) {
			return model.GPUVirtual
		}
	}
	if npuModel.MatchString(name) {
		return model.GPUNPU
	}
	if vendor == "Apple" || integratedGPUModel.MatchString(name) {
		return model.GPUIntegrated
	}
	if discreteGPUModel.MatchString(name) {
		return model.GPUDiscrete
	}
	return ""
}

type gpuHardwareType struct {
	Hardware   *bool
	Integrated *bool
	GPU        bool
	NPU        bool
}

func gpuHardwareKind(inferred string, flags gpuHardwareType) string {
	if flags.Hardware == nil {
		return ""
	}
	if !*flags.Hardware || inferred == model.GPUVirtual {
		return model.GPUVirtual
	}
	if flags.NPU || (inferred == model.GPUNPU && !flags.GPU) {
		return model.GPUNPU
	}
	if !flags.GPU || flags.Integrated == nil {
		return ""
	}
	if *flags.Integrated {
		return model.GPUIntegrated
	}
	return model.GPUDiscrete
}

// GPU tools are optional. Their failures never remove CPU/RAM/disk samples or
// require elevation. Commands and output are bounded independently of polling.
func gpuCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	processutil.HideWindow(cmd)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var output gpuOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return output.buffer.Bytes(), nil
}

type gpuOutput struct{ buffer bytes.Buffer }

func (b *gpuOutput) Write(data []byte) (int, error) {
	if b.buffer.Len()+len(data) > 64<<10 {
		return 0, errors.New("GPU sample exceeds 64 KiB")
	}
	return b.buffer.Write(data)
}

func gpuText(value string, limit int) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(value, "")))
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func gpuVendor(name string) string {
	name = strings.ToLower(name)
	for _, vendor := range []string{"NVIDIA", "AMD", "Intel", "Apple"} {
		if strings.Contains(name, strings.ToLower(vendor)) {
			return vendor
		}
	}
	if strings.Contains(name, "ati technologies") || strings.Contains(name, "advanced micro devices") || strings.Contains(name, "radeon") {
		return "AMD"
	}
	return ""
}

func parseNVIDIAGPUs(data []byte) []model.GPUInfo {
	reader := csv.NewReader(bytes.NewReader(data))
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = 4
	var result []model.GPUInfo
	for len(result) < model.MaxGPUs {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}
		gpu := model.GPUInfo{Name: gpuText(row[0], 128), Vendor: "NVIDIA"}
		gpu.Kind = gpuKind(gpu.Name, gpu.Vendor)
		if usage, err := strconv.ParseFloat(strings.TrimSpace(row[1]), 64); err == nil && !math.IsNaN(usage) && !math.IsInf(usage, 0) && usage >= 0 && usage <= 100 {
			gpu.UsagePercent = &usage
		}
		const mib = 1 << 20
		if total, err := strconv.ParseUint(strings.TrimSpace(row[2]), 10, 64); err == nil && total <= math.MaxUint64/mib {
			gpu.MemoryTotalBytes = total * mib
		}
		if used, err := strconv.ParseUint(strings.TrimSpace(row[3]), 10, 64); err == nil && used <= math.MaxUint64/mib {
			value := used * mib
			if gpu.MemoryTotalBytes == 0 || value <= gpu.MemoryTotalBytes {
				gpu.MemoryUsedBytes = &value
			}
		}
		if gpu.Valid() {
			result = append(result, gpu)
		}
	}
	return result
}

func parseDarwinGPUs(data []byte) []gpuDevice {
	var inventory struct {
		Displays []struct {
			Name  string `json:"sppci_model"`
			Model string `json:"_name"`
		} `json:"SPDisplaysDataType"`
	}
	if json.Unmarshal(data, &inventory) != nil {
		return nil
	}
	var devices []gpuDevice
	for _, display := range inventory.Displays {
		name := display.Name
		if name == "" {
			name = display.Model
		}
		info := model.GPUInfo{Name: gpuText(name, 128), Vendor: gpuVendor(name)}
		info.Kind = gpuKind(info.Name, info.Vendor)
		if info.Valid() && len(devices) < model.MaxGPUs {
			devices = append(devices, gpuDevice{info: info})
		}
	}
	return devices
}

func parseWindowsGPUs(data []byte) []gpuDevice {
	var inventory []struct {
		Name   string
		Vendor string `json:"AdapterCompatibility"`
	}
	if json.Unmarshal(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}), &inventory) != nil {
		return nil
	}
	var devices []gpuDevice
	for _, display := range inventory {
		info := model.GPUInfo{Name: gpuText(display.Name, 128), Vendor: gpuVendor(display.Name + " " + display.Vendor)}
		info.Kind = gpuKind(info.Name, info.Vendor)
		if info.Valid() && len(devices) < model.MaxGPUs {
			devices = append(devices, gpuDevice{info: info})
		}
	}
	return devices
}
