package crt

import "catalina32/pkg/cpu"

// Lookup returns the host implementation of a C library function by its Mach-O symbol name
// ("_strlen"), or nil. Games linked against system libraries at run time (pkg/source) use it.
func Lookup(sym string) cpu.ThunkFunc { return lookupCRTThunk(sym) }

// Format formats a guest printf-style string with arguments from the guest va_list at vaPtr.
func Format(c *cpu.CPU, format string, vaPtr uint32) string { return formatGuestC(c, format, vaPtr) }
