/*
 * ALL SHARE — logging.
 *
 * Two audiences. The console gets everything when debug logging is on, and
 * warnings and errors always. A bounded in-memory ring keeps the last few
 * hundred lines so the "Advanced details" panel and the diagnostics export can
 * show what happened without the user having had the console open beforehand.
 *
 * Nothing secret is ever logged: no pairing codes, no key material, no
 * clipboard contents. Device identities appear only as short fingerprints.
 */
(function (AS) {
  'use strict';

  const MAX_LINES = 400;
  const lines = [];
  let debugEnabled = false;
  const startedAt = Date.now();

  function record(level, args) {
    const entry = {
      t: Date.now(),
      level: level,
      msg: args.map(stringify).join(' ')
    };
    lines.push(entry);
    if (lines.length > MAX_LINES) lines.shift();
    return entry;
  }

  function stringify(value) {
    if (typeof value === 'string') return value;
    if (value instanceof Error) return value.name + ': ' + value.message;
    try {
      return JSON.stringify(value);
    } catch (err) {
      return String(value);
    }
  }

  function stamp(entry) {
    return '[' + ((entry.t - startedAt) / 1000).toFixed(2).padStart(7) + 's]';
  }

  const Log = {
    setDebug: function (on) { debugEnabled = !!on; },
    isDebug: function () { return debugEnabled; },

    debug: function () {
      const entry = record('debug', Array.prototype.slice.call(arguments));
      if (debugEnabled) console.debug(stamp(entry), entry.msg);
    },
    info: function () {
      const entry = record('info', Array.prototype.slice.call(arguments));
      if (debugEnabled) console.info(stamp(entry), entry.msg);
    },
    warn: function () {
      const entry = record('warn', Array.prototype.slice.call(arguments));
      console.warn(stamp(entry), entry.msg);
    },
    error: function () {
      const entry = record('error', Array.prototype.slice.call(arguments));
      console.error(stamp(entry), entry.msg);
    },

    /** The recent log as text, for the diagnostics panel. */
    dump: function () {
      return lines.map(function (entry) {
        return stamp(entry) + ' ' + entry.level.toUpperCase().padEnd(5) + ' ' + entry.msg;
      }).join('\n');
    },

    clear: function () { lines.length = 0; }
  };

  AS.Log = Log;
})(window.AllShare);
