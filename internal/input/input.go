// Package input injects mouse and keyboard events into the host machine.
//
// The interface is deliberately narrow and stateless-looking, but the
// implementations must be careful about one thing above all: never leaving a
// key or button held. A remote client can vanish mid-keystroke, and a stuck
// modifier on someone's PC is both maddening and hard to diagnose. Every
// implementation therefore tracks what it has pressed and can release it all.
package input

import "errors"

// ErrUnsupported reports an action this platform cannot perform.
var ErrUnsupported = errors.New("allshare/input: not supported on this system")

// Injector applies remote input to the local machine.
//
// Coordinates are normalized to 0..65535 across the captured surface rather
// than pixels, so a resolution change mid-session cannot misplace the pointer.
type Injector interface {
	// MoveAbsolute positions the pointer.
	MoveAbsolute(x, y uint16)
	// MoveRelative applies a raw delta, used while the client holds Pointer
	// Lock. It must not be scaled by any pointer-acceleration curve: the client
	// already asked the browser for unadjusted movement, and re-applying
	// acceleration here is what makes aiming feel wrong.
	MoveRelative(dx, dy int)
	// MouseButton presses or releases a button by index: 0 left, 1 right,
	// 2 middle, 3 back, 4 forward.
	MouseButton(index uint8, down bool)
	// Wheel scrolls, in 1/120 notch units, matching Windows WHEEL_DELTA.
	Wheel(dx, dy int)
	// Key presses or releases a key by USB HID usage ID.
	Key(usage uint16, down bool)
	// TypeText enters literal text, for the layout-matching mode and for paste.
	TypeText(text string)
	// ReleaseAll releases every key and button this injector has pressed.
	ReleaseAll()
	// SetPointerMode tells the injector whether the client is sending relative
	// or absolute positions.
	SetPointerMode(relative bool)
	// SystemAction performs a privileged host action such as Ctrl+Alt+Delete.
	SystemAction(action string) error
	// Close releases anything held and shuts the injector down.
	Close() error
}

// RectAware is implemented by injectors that need to know which part of the
// desktop the client is seeing.
//
// The client sends positions normalized against the captured surface. Without
// the capture rectangle, a click on a second monitor would be mapped onto the
// primary one.
type RectAware interface {
	SetCaptureRect(left, top, width, height int)
}

// Recorder is an Injector that records what it was asked to do instead of
// touching a machine. It backs the end-to-end tests, which need to assert that
// a key press really crossed the network and arrived intact.
type Recorder struct {
	Events      []Event
	CaptureRect [4]int
	notify      chan Event
	closed      bool
}

// EventKind classifies a recorded event.
type EventKind string

// Recorded event kinds.
const (
	EventMoveAbsolute EventKind = "moveAbsolute"
	EventMoveRelative EventKind = "moveRelative"
	EventButton       EventKind = "button"
	EventWheel        EventKind = "wheel"
	EventKey          EventKind = "key"
	EventText         EventKind = "text"
	EventReleaseAll   EventKind = "releaseAll"
	EventPointerMode  EventKind = "pointerMode"
	EventSystem       EventKind = "system"
)

// Event is one recorded injection.
type Event struct {
	Kind     EventKind
	X, Y     uint16
	DX, DY   int
	Button   uint8
	Usage    uint16
	Down     bool
	Text     string
	Relative bool
	Action   string
}

// NewRecorder builds a recording injector with a buffered notification channel.
func NewRecorder(buffer int) *Recorder {
	return &Recorder{notify: make(chan Event, buffer)}
}

// Events returns a channel of recorded events.
func (r *Recorder) Notify() <-chan Event { return r.notify }

func (r *Recorder) record(event Event) {
	r.Events = append(r.Events, event)
	select {
	case r.notify <- event:
	default:
	}
}

// MoveAbsolute records a pointer position.
func (r *Recorder) MoveAbsolute(x, y uint16) {
	r.record(Event{Kind: EventMoveAbsolute, X: x, Y: y})
}

// MoveRelative records a pointer delta.
func (r *Recorder) MoveRelative(dx, dy int) {
	r.record(Event{Kind: EventMoveRelative, DX: dx, DY: dy})
}

// MouseButton records a button transition.
func (r *Recorder) MouseButton(index uint8, down bool) {
	r.record(Event{Kind: EventButton, Button: index, Down: down})
}

// Wheel records a scroll.
func (r *Recorder) Wheel(dx, dy int) {
	r.record(Event{Kind: EventWheel, DX: dx, DY: dy})
}

// Key records a key transition.
func (r *Recorder) Key(usage uint16, down bool) {
	r.record(Event{Kind: EventKey, Usage: usage, Down: down})
}

// TypeText records typed text.
func (r *Recorder) TypeText(text string) {
	r.record(Event{Kind: EventText, Text: text})
}

// ReleaseAll records a release-everything request.
func (r *Recorder) ReleaseAll() { r.record(Event{Kind: EventReleaseAll}) }

// SetPointerMode records a pointer mode change.
func (r *Recorder) SetPointerMode(relative bool) {
	r.record(Event{Kind: EventPointerMode, Relative: relative})
}

// SetCaptureRect records the captured region, so tests can assert the mapping.
func (r *Recorder) SetCaptureRect(left, top, width, height int) {
	r.CaptureRect = [4]int{left, top, width, height}
}

// SystemAction records a system action request.
func (r *Recorder) SystemAction(action string) error {
	r.record(Event{Kind: EventSystem, Action: action})
	return nil
}

// Close marks the recorder finished.
func (r *Recorder) Close() error {
	r.closed = true
	return nil
}
