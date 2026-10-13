# UV Studio, as served by checkin-board

UV Studio is by Armel FAUVEAU, under the Apache License 2.0 (`LICENSE`,
`NOTICE`). It was copied from https://armel.github.io/uvstudio/ on
2026-10-13, version 1.8.0 (`js/studio-version.js`).

checkin-board serves it at `/uvstudio/` on UV-K5/UV-K1 nodes (admin
login), so the radio plugged into the Pi can be used from any browser on
the node's network.

## Changes from the original

- `index.html`:
  - loads `cb/serial-bridge.js` first, `cb/cb.css` after its stylesheets,
    and `cb/cb.js` before its scripts;
  - no longer loads `js/studio-flash-counter.js`, which reported every
    flash to a counter on the internet. That file isn't included.
- `cb/serial-bridge.js` (new) stands in for Web Serial. It offers one port,
  this node's radio, whose bytes travel over a WebSocket to the node.
- `cb/cb.js` (new) removes the tools that write firmware: Flash Firmware,
  Firmware Slots, Apps, External Flash and Factory Reset. The node flashes
  the radio itself instead (Admin → Station → Radio → Firmware), so a
  dropped Wi-Fi link can't interrupt a flash. It also adds a link back.
- `cb/cb.css` (new) styles those additions and hides the flash counter.
- Not included: `assets/factory/` (factory-reset images, including
  Quansheng's stock firmware), `tests/`, and the GitHub Pages extras.

Every other file is unchanged. The page's content policy keeps it on the
node, so the online firmware catalogs it can show stay empty.

## Updating

Copy the new release's files over these (keeping `cb/`, this file and the
index.html changes above). Check that `navigator.serial` is still used only
through `requestPort`, `getPorts`, `open`, `close`, `readable`, `writable`,
`getInfo` and the `connect`/`disconnect` events, which the bridge provides.
Then run `go test ./internal/web -run UVStudio` and `CB_E2E=1 go test
./internal/web -run E2EUVStudio`.
