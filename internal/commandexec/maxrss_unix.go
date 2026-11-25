//go:build !windows && !darwin

package commandexec

// Linux and the BSDs report ru_maxrss in kilobytes.
const maxRSSUnit = 1024
