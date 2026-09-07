//go:build windows

package wake

import (
	"runtime"
	"unsafe"
)

func lockOSThread()   { runtime.LockOSThread() }
func unlockOSThread() { runtime.UnlockOSThread() }

// unsafeSlicePointer returns the address of a value for a syscall argument.
func unsafeSlicePointer[T any](v *T) unsafe.Pointer { return unsafe.Pointer(v) }
