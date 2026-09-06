/*
 * ALL SHARE — keyboard mapping.
 *
 * KeyboardEvent.code names a physical key, independent of the layout the user
 * has selected. Mapping code → USB HID usage → PS/2 scancode → SendInput is
 * therefore layout-neutral in the right way: the remote PC applies its own
 * keyboard layout, exactly as it would if the user were sitting at it, and
 * games that read Raw Input or DirectInput see real scancodes rather than
 * synthesised virtual keys.
 *
 * The alternative — translating to characters here and injecting Unicode on the
 * PC — is offered as an option for typing across mismatched layouts, but it
 * cannot drive games and so is not the default.
 */
(function (AS) {
  'use strict';

  const K = {};

  /** KeyboardEvent.code → USB HID keyboard/keypad usage ID. */
  const CODE_TO_HID = {
    KeyA: 0x04, KeyB: 0x05, KeyC: 0x06, KeyD: 0x07, KeyE: 0x08, KeyF: 0x09,
    KeyG: 0x0A, KeyH: 0x0B, KeyI: 0x0C, KeyJ: 0x0D, KeyK: 0x0E, KeyL: 0x0F,
    KeyM: 0x10, KeyN: 0x11, KeyO: 0x12, KeyP: 0x13, KeyQ: 0x14, KeyR: 0x15,
    KeyS: 0x16, KeyT: 0x17, KeyU: 0x18, KeyV: 0x19, KeyW: 0x1A, KeyX: 0x1B,
    KeyY: 0x1C, KeyZ: 0x1D,

    Digit1: 0x1E, Digit2: 0x1F, Digit3: 0x20, Digit4: 0x21, Digit5: 0x22,
    Digit6: 0x23, Digit7: 0x24, Digit8: 0x25, Digit9: 0x26, Digit0: 0x27,

    Enter: 0x28, Escape: 0x29, Backspace: 0x2A, Tab: 0x2B, Space: 0x2C,
    Minus: 0x2D, Equal: 0x2E, BracketLeft: 0x2F, BracketRight: 0x30,
    Backslash: 0x31, IntlHash: 0x32, Semicolon: 0x33, Quote: 0x34,
    Backquote: 0x35, Comma: 0x36, Period: 0x37, Slash: 0x38, CapsLock: 0x39,

    F1: 0x3A, F2: 0x3B, F3: 0x3C, F4: 0x3D, F5: 0x3E, F6: 0x3F,
    F7: 0x40, F8: 0x41, F9: 0x42, F10: 0x43, F11: 0x44, F12: 0x45,

    PrintScreen: 0x46, ScrollLock: 0x47, Pause: 0x48,
    Insert: 0x49, Home: 0x4A, PageUp: 0x4B, Delete: 0x4C, End: 0x4D, PageDown: 0x4E,
    ArrowRight: 0x4F, ArrowLeft: 0x50, ArrowDown: 0x51, ArrowUp: 0x52,

    NumLock: 0x53, NumpadDivide: 0x54, NumpadMultiply: 0x55,
    NumpadSubtract: 0x56, NumpadAdd: 0x57, NumpadEnter: 0x58,
    Numpad1: 0x59, Numpad2: 0x5A, Numpad3: 0x5B, Numpad4: 0x5C, Numpad5: 0x5D,
    Numpad6: 0x5E, Numpad7: 0x5F, Numpad8: 0x60, Numpad9: 0x61,
    Numpad0: 0x62, NumpadDecimal: 0x63,

    IntlBackslash: 0x64, ContextMenu: 0x65, Power: 0x66, NumpadEqual: 0x67,

    F13: 0x68, F14: 0x69, F15: 0x6A, F16: 0x6B, F17: 0x6C, F18: 0x6D,
    F19: 0x6E, F20: 0x6F, F21: 0x70, F22: 0x71, F23: 0x72, F24: 0x73,

    IntlRo: 0x87, KanaMode: 0x88, IntlYen: 0x89, Convert: 0x8A, NonConvert: 0x8B,
    Lang1: 0x90, Lang2: 0x91,

    ControlLeft: 0xE0, ShiftLeft: 0xE1, AltLeft: 0xE2, MetaLeft: 0xE3,
    ControlRight: 0xE4, ShiftRight: 0xE5, AltRight: 0xE6, MetaRight: 0xE7,

    // Some layouts report the OS key this way.
    OSLeft: 0xE3, OSRight: 0xE7
  };

  /**
   * ChromeOS top row → F1..F10.
   *
   * A Chromebook's function row reports media actions rather than F-keys unless
   * Search is held. Windows applications expect F1..F10 there, so those codes
   * are translated by default. It is a setting, because a user who genuinely
   * wants to change the volume of the *remote* machine can turn it off.
   */
  const CHROMEOS_TOP_ROW = {
    BrowserBack: 0x3A,       // F1
    BrowserForward: 0x3B,    // F2
    BrowserRefresh: 0x3C,    // F3
    ZoomToggle: 0x3D,        // F4  (full screen)
    BrowserSearch: 0x3E,     // F5
    LaunchApp1: 0x3E,        // F5  (overview, on newer models)
    BrightnessDown: 0x3F,    // F6
    BrightnessUp: 0x40,      // F7
    AudioVolumeMute: 0x41,   // F8
    AudioVolumeDown: 0x42,   // F9
    AudioVolumeUp: 0x43,     // F10
    MediaPlayPause: 0x44,    // F11
    LaunchScreenSaver: 0x45  // F12
  };

  const HID_TO_NAME = (function () {
    const out = {};
    for (const code in CODE_TO_HID) if (!(CODE_TO_HID[code] in out)) out[CODE_TO_HID[code]] = code;
    return out;
  })();

  K.HID = {
    NONE: 0x00,
    ESCAPE: 0x29,
    TAB: 0x2B,
    DELETE: 0x4C,
    CONTROL_LEFT: 0xE0, SHIFT_LEFT: 0xE1, ALT_LEFT: 0xE2, META_LEFT: 0xE3,
    CONTROL_RIGHT: 0xE4, SHIFT_RIGHT: 0xE5, ALT_RIGHT: 0xE6, META_RIGHT: 0xE7
  };

  K.isChromeOS = function () {
    return /CrOS/.test(navigator.userAgent || '');
  };

  /**
   * Map a KeyboardEvent to a HID usage, or 0 when there is no mapping.
   *
   * options.chromebookTopRow translates the ChromeOS media row to F1..F12.
   */
  K.usageFor = function (event, options) {
    const code = event.code;
    if (!code) return 0;
    const direct = CODE_TO_HID[code];
    if (direct !== undefined) return direct;
    if (options && options.chromebookTopRow) {
      const mapped = CHROMEOS_TOP_ROW[code];
      if (mapped !== undefined) return mapped;
    }
    // Some layouts and remappers emit "Unidentified" with a usable keyCode.
    if (code === 'Unidentified' && event.keyCode) {
      const fallback = LEGACY_KEYCODE_TO_HID[event.keyCode];
      if (fallback !== undefined) return fallback;
    }
    return 0;
  };

  /** A readable name for a usage, used in the diagnostics panel. */
  K.nameFor = function (usage) {
    return HID_TO_NAME[usage] || ('HID 0x' + usage.toString(16));
  };

  K.isModifier = function (usage) {
    return usage >= 0xE0 && usage <= 0xE7;
  };

  /**
   * Keys the browser or ChromeOS would otherwise swallow.
   *
   * These are exactly the keys Keyboard Lock exists to deliver, and the reason
   * All Keys mode requires fullscreen: without it, Alt+Tab switches the
   * Chromebook's window instead of the remote PC's.
   */
  K.SYSTEM_KEYS = [
    'Escape', 'Tab', 'F1', 'F2', 'F3', 'F4', 'F5', 'F6', 'F7', 'F8', 'F9',
    'F10', 'F11', 'F12', 'MetaLeft', 'MetaRight', 'AltLeft', 'AltRight',
    'ControlLeft', 'ControlRight', 'BrowserBack', 'BrowserForward',
    'BrowserRefresh', 'ZoomToggle', 'PrintScreen'
  ];

  /**
   * Browser shortcuts a remote-desktop user almost always means for the remote
   * machine. They are prevented from acting locally while a session has focus.
   */
  K.shouldPreventDefault = function (event, allKeys) {
    if (event.code === 'F11') return true;          // our own fullscreen control
    if (event.metaKey || event.ctrlKey || event.altKey) return true;
    if (allKeys) return true;
    const passthrough = ['F5', 'F6', 'Tab', 'Escape', ' ', 'Space',
      'ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'Home', 'End',
      'PageUp', 'PageDown', 'Backspace'];
    return passthrough.indexOf(event.code) >= 0 || passthrough.indexOf(event.key) >= 0;
  };

  // A very small legacy table, only for the rare "Unidentified" case.
  const LEGACY_KEYCODE_TO_HID = {
    8: 0x2A, 9: 0x2B, 13: 0x28, 16: 0xE1, 17: 0xE0, 18: 0xE2, 20: 0x39,
    27: 0x29, 32: 0x2C, 33: 0x4B, 34: 0x4E, 35: 0x4D, 36: 0x4A,
    37: 0x50, 38: 0x52, 39: 0x4F, 40: 0x51, 45: 0x49, 46: 0x4C
  };

  K.CODE_TO_HID = CODE_TO_HID;
  K.CHROMEOS_TOP_ROW = CHROMEOS_TOP_ROW;
  AS.Keymap = K;
})(window.AllShare);
