/*
 * ALL SHARE — inline SVG icons.
 *
 * Icons are inline rather than loaded because this app runs from file://, where
 * fetching a sprite sheet or an icon font is blocked by the browser's CORS
 * rules. Inlining also means the interface never renders half-drawn while an
 * asset loads.
 *
 * All glyphs are drawn here on a 24×24 grid with a 1.8px stroke, so they sit
 * together as one set rather than looking borrowed from several.
 */
(function (AS) {
  'use strict';

  function svg(body, viewBox) {
    return '<svg viewBox="' + (viewBox || '0 0 24 24') + '" fill="none" ' +
      'stroke="currentColor" stroke-width="1.8" stroke-linecap="round" ' +
      'stroke-linejoin="round" aria-hidden="true">' + body + '</svg>';
  }

  const Icons = {
    monitor: svg('<rect x="2.5" y="4" width="19" height="13" rx="2"/><path d="M9 21h6M12 17v4"/>'),
    'monitor-lg': svg('<rect x="2.5" y="4" width="19" height="13" rx="2"/><path d="M9 21h6M12 17v4"/><path d="M7 9.5h6" opacity=".5"/><path d="M7 12.5h3.5" opacity=".5"/>'),
    plus: svg('<path d="M12 5v14M5 12h14"/>'),
    gear: svg('<circle cx="12" cy="12" r="3.2"/><path d="M19.4 15a1.6 1.6 0 0 0 .32 1.77l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.6 1.6 0 0 0-1.77-.32 1.6 1.6 0 0 0-1 1.47V21a2 2 0 1 1-4 0v-.11a1.6 1.6 0 0 0-1.05-1.47 1.6 1.6 0 0 0-1.77.32l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.6 1.6 0 0 0 .32-1.77 1.6 1.6 0 0 0-1.47-1H3a2 2 0 1 1 0-4h.11a1.6 1.6 0 0 0 1.47-1.05 1.6 1.6 0 0 0-.32-1.77l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.6 1.6 0 0 0 1.77.32H9a1.6 1.6 0 0 0 1-1.47V3a2 2 0 1 1 4 0v.11a1.6 1.6 0 0 0 1 1.47 1.6 1.6 0 0 0 1.77-.32l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.6 1.6 0 0 0-.32 1.77V9a1.6 1.6 0 0 0 1.47 1H21a2 2 0 1 1 0 4h-.11a1.6 1.6 0 0 0-1.47 1z"/>'),
    sun: svg('<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>'),
    moon: svg('<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/>'),
    power: svg('<path d="M12 3v9"/><path d="M18.4 6.6a9 9 0 1 1-12.8 0"/>'),
    pointer: svg('<path d="M5 3l6.5 17 2.4-6.9 6.9-2.4z"/>'),
    keyboard: svg('<rect x="2" y="6" width="20" height="12" rx="2"/><path d="M6 10h.01M10 10h.01M14 10h.01M18 10h.01M8 14h8"/>'),
    clipboard: svg('<rect x="8" y="3" width="8" height="4" rx="1"/><path d="M9 5H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7a2 2 0 0 0-2-2h-3"/>'),
    volume: svg('<path d="M11 5L6.5 9H3v6h3.5L11 19z"/><path d="M15.5 8.5a5 5 0 0 1 0 7"/><path d="M18.5 5.5a9 9 0 0 1 0 13"/>'),
    'volume-off': svg('<path d="M11 5L6.5 9H3v6h3.5L11 19z"/><path d="M22 9l-6 6M16 9l6 6"/>'),
    sliders: svg('<path d="M4 6h10M18 6h2M4 12h4M12 12h8M4 18h10M18 18h2"/><circle cx="16" cy="6" r="2"/><circle cx="10" cy="12" r="2"/><circle cx="16" cy="18" r="2"/>'),
    displays: svg('<rect x="2" y="5" width="12" height="9" rx="1.5"/><rect x="11" y="10" width="11" height="8" rx="1.5" fill="var(--bg-1)"/>'),
    pulse: svg('<path d="M3 12h4l2.5-7 4 14 2.5-7h5"/>'),
    expand: svg('<path d="M8 3H5a2 2 0 0 0-2 2v3M16 3h3a2 2 0 0 1 2 2v3M8 21H5a2 2 0 0 1-2-2v-3M16 21h3a2 2 0 0 0 2-2v-3"/>'),
    collapse: svg('<path d="M4 9h5V4M20 9h-5V4M4 15h5v5M20 15h-5v5"/>'),
    link: svg('<path d="M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7-7l-1.7 1.7"/><path d="M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7 7l1.7-1.7"/>'),
    check: svg('<path d="M4 12.5l5.5 5.5L20 6.5"/>'),
    x: svg('<path d="M6 6l12 12M18 6L6 18"/>'),
    alert: svg('<path d="M12 8v5M12 17h.01"/><path d="M10.3 3.9L2.4 17.5A2 2 0 0 0 4.1 20.5h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/>'),
    info: svg('<circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8h.01"/>'),
    wake: svg('<path d="M13 2L4.5 13.5H11L9.5 22l9-12H12z"/>'),
    dots: svg('<circle cx="12" cy="5" r="1.4" fill="currentColor" stroke="none"/><circle cx="12" cy="12" r="1.4" fill="currentColor" stroke="none"/><circle cx="12" cy="19" r="1.4" fill="currentColor" stroke="none"/>'),
    trash: svg('<path d="M4 7h16M10 11v6M14 11v6"/><path d="M6 7l1 13a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-13"/><path d="M9 7V4h6v3"/>'),
    shield: svg('<path d="M12 2l8 3.5v6c0 5-3.4 9.3-8 10.5-4.6-1.2-8-5.5-8-10.5v-6z"/><path d="M9 12l2 2 4-4"/>'),
    gamepad: svg('<path d="M7 11h4M9 9v4M15.5 11h.01M18 13h.01"/><path d="M17.5 6.5h-11A4.5 4.5 0 0 0 2 11v.5A5.5 5.5 0 0 0 7.5 17c1.6 0 2.3-.7 3-1.4h3c.7.7 1.4 1.4 3 1.4A5.5 5.5 0 0 0 22 11.5V11a4.5 4.5 0 0 0-4.5-4.5z"/>'),
    desktop: svg('<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/><path d="M6.5 8h5M6.5 11h3"/>'),
    scale: svg('<path d="M3 8V5a2 2 0 0 1 2-2h3M21 8V5a2 2 0 0 0-2-2h-3M3 16v3a2 2 0 0 0 2 2h3M21 16v3a2 2 0 0 1-2 2h-3"/><rect x="8" y="8" width="8" height="8" rx="1"/>'),
    refresh: svg('<path d="M21 12a9 9 0 1 1-2.6-6.4"/><path d="M21 4v5h-5"/>')
  };

  /** Return the markup for an icon, or an empty string when unknown. */
  Icons.get = function (name) { return Icons[name] || ''; };

  /** Fill every element in root that declares a data-icon. */
  Icons.hydrate = function (root) {
    const scope = root || document;
    const nodes = scope.querySelectorAll('[data-icon]');
    for (const node of nodes) {
      const markup = Icons.get(node.getAttribute('data-icon'));
      // The markup here is entirely from the table above, never from a remote
      // device, so assigning it as HTML is safe.
      if (markup) node.innerHTML = markup;
    }
  };

  AS.Icons = Icons;
})(window.AllShare);
