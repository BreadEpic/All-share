//go:build !windows

package input

// New returns an injector for this platform.
//
// Only Windows can drive a real desktop; on other platforms the agent still
// runs — which is what lets the whole system be exercised end to end in a test
// environment — but input is recorded rather than applied.
func New() (Injector, error) {
	return NewRecorder(256), nil
}
