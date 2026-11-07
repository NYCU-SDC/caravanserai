//go:build linux

// Package sysinfo collects host-level system information (CPU, memory, disk,
// kernel, OS) and converts it to Caravanserai API types. It has no dependencies
// on the Agent runtime so it can be unit-tested in isolation.
package sysinfo

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"syscall"

	v1 "NYCU-SDC/caravanserai/api/v1"
)

// System-reserved amounts subtracted from Capacity to derive Allocatable.
const (
	ReservedCPUMillicores = 500 // 0.5 core for OS + agent
	ReservedMemoryMi      = 512 // 512 MiB for OS + agent
)

// SystemInfo holds raw system metrics collected from the host.
type SystemInfo struct {
	CPUMillicores int64  // e.g. 16000 for 16 cores
	MemoryTotalMi int64  // total physical memory in MiB
	DiskTotalGi   int64  // total disk space on dockerRoot partition in GiB
	DiskAvailGi   int64  // available disk space in GiB
	KernelVersion string // e.g. "Linux 5.15.0-84-generic"
	OSImage       string // e.g. "Ubuntu 22.04.3 LTS"
}

// Collect gathers system information from the current host.
// dockerRoot is the Docker data-root directory (e.g. "/var/lib/docker") used
// to determine disk statistics for the correct filesystem.
func Collect(dockerRoot string) (SystemInfo, error) {
	var info SystemInfo

	// CPU: logical cores * 1000 millicores.
	info.CPUMillicores = int64(runtime.NumCPU()) * 1000

	// Memory: parse /proc/meminfo for MemTotal.
	memMi, err := readMemInfo("/proc/meminfo")
	if err != nil {
		return SystemInfo{}, fmt.Errorf("sysinfo: read meminfo: %w", err)
	}
	info.MemoryTotalMi = memMi

	// Disk: statfs on the dockerRoot partition.
	totalGi, availGi, err := statfsDisk(dockerRoot)
	if err != nil {
		return SystemInfo{}, fmt.Errorf("sysinfo: statfs %q: %w", dockerRoot, err)
	}
	info.DiskTotalGi = totalGi
	info.DiskAvailGi = availGi

	// Kernel: syscall.Uname -> "Linux 5.15.0-84-generic".
	kernel, err := readKernelVersion()
	if err != nil {
		return SystemInfo{}, fmt.Errorf("sysinfo: read kernel version: %w", err)
	}
	info.KernelVersion = kernel

	// OS: parse /etc/os-release for PRETTY_NAME.
	osImage, err := readOSRelease("/etc/os-release")
	if err != nil {
		return SystemInfo{}, fmt.Errorf("sysinfo: read os-release: %w", err)
	}
	info.OSImage = osImage

	return info, nil
}

// ToCapacity returns the ResourceList representing raw physical totals.
func (s SystemInfo) ToCapacity() v1.ResourceList {
	return v1.ResourceList{
		"cpu":    fmt.Sprintf("%dm", s.CPUMillicores),
		"memory": fmt.Sprintf("%dMi", s.MemoryTotalMi),
		"disk":   fmt.Sprintf("%dGi", s.DiskTotalGi),
	}
}

// ToAllocatable returns the ResourceList after subtracting system reserves.
// Reserved: 500m CPU, 512Mi memory. Disk uses the actual available space
// (from Bavail), which already accounts for used space and the ext4
// reserved-block percentage.
func (s SystemInfo) ToAllocatable() v1.ResourceList {
	cpuAlloc := s.CPUMillicores - ReservedCPUMillicores
	if cpuAlloc < 0 {
		cpuAlloc = 0
	}

	memAlloc := s.MemoryTotalMi - ReservedMemoryMi
	if memAlloc < 0 {
		memAlloc = 0
	}

	return v1.ResourceList{
		"cpu":    fmt.Sprintf("%dm", cpuAlloc),
		"memory": fmt.Sprintf("%dMi", memAlloc),
		"disk":   fmt.Sprintf("%dGi", s.DiskAvailGi),
	}
}

// ToNodeInfo returns the NodeInfo struct populated with static system details.
func (s SystemInfo) ToNodeInfo(agentVersion string) v1.NodeInfo {
	return v1.NodeInfo{
		KernelVersion: s.KernelVersion,
		OSImage:       s.OSImage,
		AgentVersion:  agentVersion,
	}
}

// readMemInfo reads /proc/meminfo from the given path and returns MemTotal in MiB.
func readMemInfo(path string) (int64, error) {
	f, err := openFile(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	return parseMemInfo(f)
}

// parseMemInfo extracts MemTotal (in MiB) from /proc/meminfo content.
// The file format has lines like: "MemTotal:       16304208 kB"
func parseMemInfo(r io.Reader) (int64, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}

		// Parse: "MemTotal:       16304208 kB"
		var label string
		var valueKB int64
		var unit string
		n, err := fmt.Sscanf(line, "%s %d %s", &label, &valueKB, &unit)
		if err != nil || n != 3 {
			return 0, fmt.Errorf("parse meminfo line %q: %w", line, err)
		}

		// Convert kB to MiB (1 MiB = 1024 kB).
		return valueKB / 1024, nil
	}

	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan meminfo: %w", err)
	}

	return 0, fmt.Errorf("MemTotal not found in meminfo")
}

// readOSRelease reads /etc/os-release from the given path and returns
// the PRETTY_NAME value.
func readOSRelease(path string) (string, error) {
	f, err := openFile(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	return parseOSRelease(f)
}

// parseOSRelease extracts the PRETTY_NAME value from /etc/os-release content.
func parseOSRelease(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "PRETTY_NAME=") {
			continue
		}

		value := strings.TrimPrefix(line, "PRETTY_NAME=")
		// Strip surrounding quotes if present.
		value = strings.Trim(value, "\"")
		return value, nil
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan os-release: %w", err)
	}

	return "", fmt.Errorf("PRETTY_NAME not found in os-release")
}

// readKernelVersion returns "Sysname Release" from syscall.Uname().
func readKernelVersion() (string, error) {
	var uname syscall.Utsname
	if err := syscall.Uname(&uname); err != nil {
		return "", fmt.Errorf("uname: %w", err)
	}

	sysname := charsToString(uname.Sysname[:])
	release := charsToString(uname.Release[:])
	return sysname + " " + release, nil
}

// statfsDisk returns the total and available disk space (in GiB) for the
// filesystem containing path.
func statfsDisk(path string) (totalGi, availGi int64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}

	bsize := uint64(stat.Bsize)
	totalBytes := stat.Blocks * bsize
	availBytes := stat.Bavail * bsize

	const gib = 1 << 30
	return int64(totalBytes / gib), int64(availBytes / gib), nil
}

// charsToString converts a null-terminated int8 array (from syscall.Utsname)
// to a Go string.
func charsToString(arr []int8) string {
	buf := make([]byte, 0, len(arr))
	for _, c := range arr {
		if c == 0 {
			break
		}
		buf = append(buf, byte(c))
	}
	return string(buf)
}

// openFile is a thin wrapper around os.Open.
func openFile(path string) (io.ReadCloser, error) {
	return os.Open(path)
}
