//go:build linux

package sysinfo

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// parseMemInfo
// ---------------------------------------------------------------------------

func TestParseMemInfo(t *testing.T) {
	t.Helper()

	const sample = `MemTotal:       16304208 kB
MemFree:         1234567 kB
MemAvailable:    8765432 kB
Buffers:          123456 kB
Cached:          4567890 kB
`

	mi, err := parseMemInfo(strings.NewReader(sample))
	require.NoError(t, err)
	// 16304208 kB / 1024 = 15922 MiB (integer division).
	assert.Equal(t, int64(15922), mi)
}

func TestParseMemInfoSmallValue(t *testing.T) {
	t.Helper()

	const sample = `MemTotal:        1024 kB
MemFree:          512 kB
`
	mi, err := parseMemInfo(strings.NewReader(sample))
	require.NoError(t, err)
	assert.Equal(t, int64(1), mi) // 1024 / 1024 = 1
}

func TestParseMemInfoMissing(t *testing.T) {
	t.Helper()

	const sample = `MemFree:         1234567 kB
MemAvailable:    8765432 kB
`
	_, err := parseMemInfo(strings.NewReader(sample))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MemTotal not found")
}

func TestParseMemInfoEmpty(t *testing.T) {
	t.Helper()

	_, err := parseMemInfo(strings.NewReader(""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MemTotal not found")
}

// ---------------------------------------------------------------------------
// parseOSRelease
// ---------------------------------------------------------------------------

func TestParseOSRelease(t *testing.T) {
	t.Helper()

	const sample = `NAME="Ubuntu"
VERSION="22.04.3 LTS (Jammy Jellyfish)"
ID=ubuntu
PRETTY_NAME="Ubuntu 22.04.3 LTS"
VERSION_ID="22.04"
`

	name, err := parseOSRelease(strings.NewReader(sample))
	require.NoError(t, err)
	assert.Equal(t, "Ubuntu 22.04.3 LTS", name)
}

func TestParseOSReleaseUnquoted(t *testing.T) {
	t.Helper()

	const sample = `NAME=Arch Linux
PRETTY_NAME=Arch Linux
`

	name, err := parseOSRelease(strings.NewReader(sample))
	require.NoError(t, err)
	assert.Equal(t, "Arch Linux", name)
}

func TestParseOSReleaseMissing(t *testing.T) {
	t.Helper()

	const sample = `NAME="Ubuntu"
VERSION="22.04.3 LTS"
`

	_, err := parseOSRelease(strings.NewReader(sample))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PRETTY_NAME not found")
}

func TestParseOSReleaseEmpty(t *testing.T) {
	t.Helper()

	_, err := parseOSRelease(strings.NewReader(""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PRETTY_NAME not found")
}

// ---------------------------------------------------------------------------
// ToCapacity
// ---------------------------------------------------------------------------

func TestToCapacity(t *testing.T) {
	t.Helper()

	info := SystemInfo{
		CPUMillicores: 16000,
		MemoryTotalMi: 64000,
		DiskTotalGi:   640,
	}

	cap := info.ToCapacity()
	assert.Equal(t, "16000m", cap["cpu"])
	assert.Equal(t, "64000Mi", cap["memory"])
	assert.Equal(t, "640Gi", cap["disk"])
}

func TestToCapacitySingleCore(t *testing.T) {
	t.Helper()

	info := SystemInfo{
		CPUMillicores: 1000,
		MemoryTotalMi: 512,
		DiskTotalGi:   20,
	}

	cap := info.ToCapacity()
	assert.Equal(t, "1000m", cap["cpu"])
	assert.Equal(t, "512Mi", cap["memory"])
	assert.Equal(t, "20Gi", cap["disk"])
}

// ---------------------------------------------------------------------------
// ToAllocatable
// ---------------------------------------------------------------------------

func TestToAllocatable(t *testing.T) {
	t.Helper()

	info := SystemInfo{
		CPUMillicores: 16000,
		MemoryTotalMi: 64000,
		DiskTotalGi:   640,
		DiskAvailGi:   580,
	}

	alloc := info.ToAllocatable()
	assert.Equal(t, "15500m", alloc["cpu"])     // 16000 - 500
	assert.Equal(t, "63488Mi", alloc["memory"]) // 64000 - 512
	assert.Equal(t, "580Gi", alloc["disk"])     // DiskAvailGi directly
}

func TestToAllocatableClampToZero(t *testing.T) {
	t.Helper()

	info := SystemInfo{
		CPUMillicores: 200, // less than 500 reserved
		MemoryTotalMi: 256, // less than 512 reserved
		DiskAvailGi:   1,
	}

	alloc := info.ToAllocatable()
	assert.Equal(t, "0m", alloc["cpu"])
	assert.Equal(t, "0Mi", alloc["memory"])
	assert.Equal(t, "1Gi", alloc["disk"])
}

// ---------------------------------------------------------------------------
// ToNodeInfo
// ---------------------------------------------------------------------------

func TestToNodeInfo(t *testing.T) {
	t.Helper()

	info := SystemInfo{
		KernelVersion: "Linux 5.15.0-84-generic",
		OSImage:       "Ubuntu 22.04.3 LTS",
	}

	ni := info.ToNodeInfo("v1.2.0")
	assert.Equal(t, "Linux 5.15.0-84-generic", ni.KernelVersion)
	assert.Equal(t, "Ubuntu 22.04.3 LTS", ni.OSImage)
	assert.Equal(t, "v1.2.0", ni.AgentVersion)
}

// ---------------------------------------------------------------------------
// Collect — error case
// ---------------------------------------------------------------------------

func TestCollectNonExistentPath(t *testing.T) {
	t.Helper()

	_, err := Collect("/nonexistent/path/that/does/not/exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sysinfo:")
}
