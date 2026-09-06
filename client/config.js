/*
 * ALL SHARE — local configuration (optional)
 *
 * Everything here can also be set from the app's own setup screen, so editing
 * this file is never required. It exists so that a PC or an administrator can
 * ship a client folder that is already pointed at the right service.
 *
 * Values set here become the defaults; anything the user changes in Settings
 * wins over them.
 */
window.ALLSHARE_CONFIG = {
  // WebSocket address of your ALL SHARE service, for example:
  //   "wss://allshare.example.com/rv"
  // Leave empty to have the app ask for it on first run.
  rendezvous: "",

  // Optional friendly name for this device, shown on the PC when pairing.
  // Leave empty to let the app suggest one.
  deviceLabel: ""
};
